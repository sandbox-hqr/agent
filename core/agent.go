// Package core is the agent's plain-Go core loop — enroll, heartbeat,
// long-poll, execute, report, plus the RTB tunnel session client. None of
// it uses goose: goose is an inbound HTTP server framework, and this
// loop's only inbound surface is the tiny local health API (../localapi),
// which is the sole place goose appears in this binary (draft/
// micro-machine.md §3).
package core

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sandbox-hqr/agent/driver"
)

// Agent owns no authoritative state — the scheduler bundle's Postgres
// store is the source of truth; Agent's local State is just enough to
// survive its own restart (draft/micro-machine.md §1).
type Agent struct {
	cfg       *Config
	state     *State
	scheduler SchedulerClient
	tunnel    TunnelClient
	driver    driver.Driver
	log       *slog.Logger

	tunnelDegraded atomic.Bool
	tunnelStreams  atomic.Int64

	sessionMu sync.Mutex
	session   Session
}

func NewAgent(cfg *Config, state *State, scheduler SchedulerClient, tunnel TunnelClient, drv driver.Driver, log *slog.Logger) *Agent {
	return &Agent{cfg: cfg, state: state, scheduler: scheduler, tunnel: tunnel, driver: drv, log: log}
}

// Run is the daemon entrypoint: ensure enrolled, reconcile local driver
// state against ground truth, then run the heartbeat, command, and tunnel
// loops concurrently until ctx is cancelled. Failure modes degrade
// independently (draft/micro-machine.md §7) — losing the RTB session
// never stops heartbeating or command execution.
func (a *Agent) Run(ctx context.Context) error {
	if err := a.ensureEnrolled(ctx); err != nil {
		return fmt.Errorf("enrollment: %w", err)
	}
	a.reconcileOnStartup(ctx)

	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); a.heartbeatLoop(ctx) }()
	go func() { defer wg.Done(); a.commandLoop(ctx) }()
	go func() { defer wg.Done(); a.tunnelLoop(ctx) }()
	wg.Wait()
	return nil
}

func (a *Agent) ensureEnrolled(ctx context.Context) error {
	if a.state.Enrolled() {
		a.log.Info("already enrolled", "node_id", a.state.NodeID)
		return nil
	}
	if a.cfg.JoinToken == "" {
		return fmt.Errorf("not enrolled and no join token configured (SANDBOX_AGENT_JOIN_TOKEN)")
	}

	capacity := probeCapacity()
	creds, err := a.scheduler.Enroll(ctx, a.cfg.JoinToken, capacity, hostname())
	if err != nil {
		return err
	}
	if err := a.state.SetCredentials(creds.NodeID, creds.BearerToken); err != nil {
		return err
	}
	a.log.Info("enrolled", "node_id", creds.NodeID)
	return nil
}

// reconcileOnStartup calls Driver.List to rediscover Spaces that survived
// an agent crash or host reboot, per draft/micro-machine.md §7 — the next
// heartbeat reports this actual state rather than an assumed empty view.
func (a *Agent) reconcileOnStartup(ctx context.Context) {
	states, err := a.driver.List(ctx)
	if err != nil {
		a.log.Error("startup reconciliation: Driver.List failed", "error", err)
		return
	}
	a.log.Info("startup reconciliation complete", "running_spaces", len(states))
}

func (a *Agent) heartbeatLoop(ctx context.Context) {
	bo := newBackoff(a.cfg.RetryBackoffMin, a.cfg.RetryBackoffMax)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		states, err := a.driver.List(ctx)
		var spaceIDs []string
		var allocated driver.Resources
		if err == nil {
			spaceIDs = make([]string, 0, len(states))
			for _, s := range states {
				spaceIDs = append(spaceIDs, s.ID)
			}
			// Sum actual per-instance usage as a defense-in-depth cross-check
			// against the scheduler's own accounting (draft/micro-machine.md §5).
			for _, s := range states {
				if stats, statErr := a.driver.Stats(ctx, s.ID); statErr == nil {
					allocated.MemMB += stats.MemUsedMB
					allocated.DiskGB += stats.DiskUsedGB
				}
			}
		}

		_, err = a.scheduler.Heartbeat(ctx, a.state.NodeID, HeartbeatReport{
			Allocated: allocated,
			SpaceIDs:  spaceIDs,
		})
		if err != nil {
			a.log.Warn("heartbeat failed", "error", err)
			sleep(ctx, bo.Next())
			continue
		}
		bo.Reset()
		sleep(ctx, a.cfg.HeartbeatInterval)
	}
}

func (a *Agent) commandLoop(ctx context.Context) {
	bo := newBackoff(a.cfg.RetryBackoffMin, a.cfg.RetryBackoffMax)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		cmd, err := a.scheduler.NextCommand(ctx, a.state.NodeID, a.cfg.LongpollTimeout)
		if err != nil {
			// Only transport/HTTP errors trigger backoff here — an ordinary
			// empty long-poll return is `cmd == nil, err == nil` and is a
			// *successful* call (draft/micro-machine.md §7).
			a.log.Warn("long-poll failed", "error", err)
			sleep(ctx, bo.Next())
			continue
		}
		bo.Reset()
		if cmd == nil {
			continue // ordinary timeout, poll again immediately
		}

		a.executeCommand(ctx, cmd)
	}
}

// executeCommand is idempotent against redelivery — checks the local
// execution log before acting, so a redelivered command (e.g. after a
// result-reporting timeout that actually succeeded) checks existing state
// first instead of blindly re-executing (draft/micro-machine.md §7).
func (a *Agent) executeCommand(ctx context.Context, cmd *Command) {
	logger := a.log.With("command_id", cmd.ID, "type", cmd.Type)

	if a.state.AlreadyExecuted(cmd.ID) {
		logger.Info("command already executed, skipping re-execution, re-reporting success")
		_ = a.scheduler.ReportResult(ctx, a.state.NodeID, CommandResult{CommandID: cmd.ID, Success: true})
		return
	}

	var result any
	var execErr error

	switch cmd.Type {
	case "create_space":
		if !a.driver.Supports(cmd.Payload) {
			execErr = fmt.Errorf("driver %s does not support requested resources", a.driver.Name())
			break
		}
		var inst *driver.Instance
		inst, execErr = a.driver.Create(ctx, cmd.Payload)
		if execErr == nil {
			result = inst
		}
	case "delete_space":
		execErr = a.driver.Delete(ctx, cmd.Payload.SpaceID)
	default:
		execErr = fmt.Errorf("unknown command type %q", cmd.Type)
	}

	success := execErr == nil
	if execErr != nil {
		logger.Error("command execution failed", "error", execErr)
		result = map[string]string{"error": execErr.Error()}
	} else {
		logger.Info("command executed")
		if err := a.state.MarkExecuted(cmd.ID); err != nil {
			logger.Warn("failed to persist idempotency log entry", "error", err)
		}
	}

	if err := a.scheduler.ReportResult(ctx, a.state.NodeID, CommandResult{
		CommandID: cmd.ID, Success: success, Result: result,
	}); err != nil {
		logger.Warn("failed to report command result", "error", err)
	}
}

// tunnelLoop maintains one persistent multiplexed session to RTB, serving
// dial-back requests for any export targeting this Node's Spaces. Losing
// this session never stops heartbeating or command execution — it marks
// tunnelDegraded (surfaced in /readyz) and keeps retrying independently
// (draft/micro-machine.md §7).
func (a *Agent) tunnelLoop(ctx context.Context) {
	bo := newBackoff(a.cfg.RetryBackoffMin, a.cfg.RetryBackoffMax)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		session, err := a.tunnel.EstablishSession(ctx, a.state.NodeID, a.state.BearerToken)
		if err != nil {
			a.tunnelDegraded.Store(true)
			a.log.Warn("tunnel session establish failed", "error", err)
			sleep(ctx, bo.Next())
			continue
		}

		a.sessionMu.Lock()
		a.session = session
		a.sessionMu.Unlock()
		a.tunnelDegraded.Store(false)
		bo.Reset()
		a.log.Info("tunnel session established")

		a.serveSession(ctx, session) // blocks until the session dies

		a.sessionMu.Lock()
		a.session = nil
		a.sessionMu.Unlock()
		a.tunnelDegraded.Store(true)
	}
}

func (a *Agent) serveSession(ctx context.Context, session Session) {
	for {
		stream, err := session.Accept(ctx)
		if err != nil {
			return // session dead or ctx cancelled — tunnelLoop reconnects
		}
		go a.serveStream(stream)
	}
}

func (a *Agent) serveStream(stream Stream) {
	defer stream.Close()
	a.tunnelStreams.Add(1)
	defer a.tunnelStreams.Add(-1)

	header, err := decodeStreamHeader(stream)
	if err != nil {
		a.log.Warn("failed to decode stream header", "error", err)
		return
	}

	target, err := net.DialTimeout(header.Protocol, fmt.Sprintf("%s:%d", header.TargetHost, header.TargetPort), 5*time.Second)
	if err != nil {
		a.log.Warn("failed to dial guest target", "export_id", header.ExportID, "error", err)
		return
	}
	defer target.Close()

	splice(stream, target)
}

// TunnelDegraded surfaces in /readyz (localapi) per draft/micro-machine.md §9.
func (a *Agent) TunnelDegraded() bool { return a.tunnelDegraded.Load() }
func (a *Agent) TunnelStreamsActive() int64 { return a.tunnelStreams.Load() }
func (a *Agent) Enrolled() bool { return a.state.Enrolled() }

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

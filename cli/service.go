package cli

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"

	"github.com/awesome-goose/goose"
	gooselog "github.com/awesome-goose/goose/log"
	gooselogformatters "github.com/awesome-goose/goose/log/formatters"
	gooselogmodifiers "github.com/awesome-goose/goose/log/modifiers"
	gooselogprocessors "github.com/awesome-goose/goose/log/processors"
	gooseapi "github.com/awesome-goose/goose/platforms/api"
	goosetypes "github.com/awesome-goose/goose/types"
	kservice "github.com/kardianos/service"

	"github.com/sandbox-hq/agent/core"
	"github.com/sandbox-hq/agent/driver"
	"github.com/sandbox-hq/agent/localapi"
)

const (
	ServiceName        = "sandbox-agent"
	ServiceDisplayName = "Sandbox Agent"
	ServiceDescription = "Runs microVMs on this host as part of a sandbox pool."
)

// program adapts the daemon (Agent core loop + local health API) to
// kardianos/service's Interface — Start/Stop must not block. svc.Run()
// (dispatch.go's runService) is what actually invokes these; on Windows
// this integration is what lets the process correctly speak the Service
// Control Manager protocol instead of just running as a bare subprocess.
type program struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func newProgram() *program {
	return &program{done: make(chan struct{})}
}

func (p *program) Start(s kservice.Service) error {
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	go p.runDaemon(ctx)
	return nil
}

func (p *program) Stop(s kservice.Service) error {
	if p.cancel != nil {
		p.cancel()
	}
	<-p.done
	return nil
}

func (p *program) runDaemon(ctx context.Context) {
	defer close(p.done)
	logger := slog.Default()
	cfg := core.LoadConfig()

	if cfg.ControlPlaneBaseURL == "" {
		logger.Error("SANDBOX_AGENT_CONTROL_PLANE_URL is not set — run `agent cli install` first")
		return
	}

	state, err := core.LoadOrNewState(cfg.StatePath())
	if err != nil {
		logger.Error("load state", "error", err)
		return
	}

	scheduler := core.NewHTTPSchedulerClient(cfg.ControlPlaneBaseURL)
	if state.Enrolled() {
		scheduler.BearerToken = state.BearerToken
	}
	tunnel := core.NewHTTPTunnelClient(cfg.RTBBaseURL + "/v1/tunnel/session")
	drv := buildDriver(cfg.DriverType)
	agent := core.NewAgent(cfg, state, &bearerSyncingScheduler{HTTPSchedulerClient: scheduler, state: state}, tunnel, drv, logger)

	errCh := make(chan error, 2)
	go func() { errCh <- agent.Run(ctx) }()
	go func() { errCh <- runLocalAPI(ctx, cfg, agent) }()

	select {
	case <-ctx.Done():
	case err := <-errCh:
		logger.Error("daemon exited with error", "error", err)
	}
}

func serviceConfig() *kservice.Config {
	exe, _ := os.Executable()
	return &kservice.Config{
		Name:        ServiceName,
		DisplayName: ServiceDisplayName,
		Description: ServiceDescription,
		Executable:  exe,
		Arguments:   []string{"run"},
	}
}

func runLocalAPI(ctx context.Context, cfg *core.Config, agent *core.Agent) error {
	host, port := splitHostPort(cfg.LocalAPIBindAddr)
	platform := gooseapi.NewPlatform(gooseapi.WithName("sandbox-agent-localapi"), gooseapi.WithHost(host), gooseapi.WithPort(port))
	root := localapi.NewModule(agent)

	initializers := []func(container goosetypes.Container) error{logInitializer()}

	stop, err := goose.Start(goose.API(platform, root, initializers))
	if err != nil {
		return err
	}
	<-ctx.Done()
	return stop()
}

// logInitializer works around a real bug in awesome-goose/goose's default
// services chain (core/services.go registers the logger-slice producer
// under the named log.AppLoggers, but the types.Log constructor asks for
// the unnamed []*log.Logger — different reflect.Types, so DI can never
// resolve it and every goose app fails to boot on default logging). Left
// unfixed in the framework per an explicit decision — see
// ~/Projects/awesome-goose/goose/BUGS.md #1 — so every goose instance in
// this codebase supplies its own Log initializer instead.
func logInitializer() func(goosetypes.Container) error {
	return func(container goosetypes.Container) error {
		return container.Register(func() goosetypes.Log {
			return gooselog.NewLog(
				gooselog.AppLogChannel("std"),
				gooselog.NewLogger(
					[]goosetypes.Modifier{
						gooselogmodifiers.NewUUID(),
						gooselogmodifiers.NewColorTagsModifier(),
						gooselogmodifiers.NewSystemInfo(),
						gooselogmodifiers.NewStackTrace(),
					},
					gooselogformatters.NewJSON(),
					gooselogprocessors.NewConsole(),
				),
			)
		}, "", true)
	}
}

func buildDriver(driverType string) driver.Driver {
	switch driverType {
	case "firecracker":
		return driver.NewFirecracker()
	case "cloud-hypervisor":
		return driver.NewCloudHypervisor()
	default:
		return driver.NewMock()
	}
}

func splitHostPort(addr string) (string, int) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return "127.0.0.1", 9090
	}
	var port int
	fmt.Sscanf(portStr, "%d", &port)
	return host, port
}

// bearerSyncingScheduler keeps HTTPSchedulerClient.BearerToken current
// after Enroll persists new credentials to State — HTTPSchedulerClient
// itself has no reference to State (kept decoupled, testable independent
// of the local filesystem).
type bearerSyncingScheduler struct {
	*core.HTTPSchedulerClient
	state *core.State
}

func (s *bearerSyncingScheduler) Enroll(ctx context.Context, joinToken string, capacity driver.Resources, hostname string) (core.AgentCredentials, error) {
	creds, err := s.HTTPSchedulerClient.Enroll(ctx, joinToken, capacity, hostname)
	if err != nil {
		return creds, err
	}
	s.HTTPSchedulerClient.BearerToken = creds.BearerToken
	return creds, nil
}

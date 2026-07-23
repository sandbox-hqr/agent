// Package localapi is the agent's one inbound HTTP surface — a small
// goose.API instance for host-level supervision and local Prometheus
// scraping, bound to 127.0.0.1 by default (draft/micro-machine.md §3, §10).
// This is the only place goose appears in the agent binary; the core loop
// itself is plain Go and makes no inbound HTTP calls at all.
package localapi

import (
	"fmt"

	"github.com/awesome-goose/goose/io/output"
	"github.com/awesome-goose/goose/types"
	"github.com/sandbox-hq/agent/core"
)

type Controller struct {
	agent *core.Agent `inject:""`
}

type EmptyDto struct{}

// Healthz reports process-alive only (draft/micro-machine.md §9).
func (c *Controller) Healthz(_ *EmptyDto) types.Output {
	return output.JSON(map[string]any{"status": "ok"})
}

// Readyz reports enrolled + heartbeating + not in a hard-failure state.
// tunnel_degraded is surfaced here rather than failing readiness outright
// — losing the RTB session doesn't mean the agent should be considered
// unready for everything else (draft/micro-machine.md §7, §9).
func (c *Controller) Readyz(_ *EmptyDto) types.Output {
	ready := c.agent.Enrolled()
	code := 200
	if !ready {
		code = 503
	}
	return output.JSONWithCode(map[string]any{
		"enrolled":         ready,
		"tunnel_degraded":  c.agent.TunnelDegraded(),
	}, code)
}

// Metrics exposes the Prometheus-text-format metrics from draft/
// micro-machine.md §9 that are cheap to report without deeper Driver
// instrumentation (per-VM boot-latency histograms etc. are a follow-up).
func (c *Controller) Metrics(_ *EmptyDto) types.Output {
	body := fmt.Sprintf(
		"# HELP agent_tunnel_session_state 0=down, 1=connecting, 2=up\n# TYPE agent_tunnel_session_state gauge\nagent_tunnel_session_state %d\n"+
			"# HELP agent_tunnel_streams_active Open dial-back streams\n# TYPE agent_tunnel_streams_active gauge\nagent_tunnel_streams_active %d\n",
		tunnelStateValue(c.agent.TunnelDegraded()), c.agent.TunnelStreamsActive(),
	)
	return &plainTextOutput{body: body}
}

func tunnelStateValue(degraded bool) int {
	if degraded {
		return 0
	}
	return 2
}

// plainTextOutput is a minimal types.Output for the Prometheus text
// format — io/output only ships JSON/HTML/file/redirect/console builders,
// none of which fit "raw text body, no envelope".
type plainTextOutput struct{ body string }

func (o *plainTextOutput) Data() any                  { return o.body }
func (o *plainTextOutput) Code() int                  { return 200 }
func (o *plainTextOutput) Headers() map[string]string { return nil }
func (o *plainTextOutput) ContentType() string        { return "text/plain; version=0.0.4" }

package localapi

import (
	"github.com/awesome-goose/goose/modules/router"
	"github.com/awesome-goose/goose/types"
	"github.com/sandbox-hq/agent/core"
)

// Module wraps an already-constructed *core.Agent (built in main.go) as a
// goose Module — same pattern as gox-apps/libs/reverse-tunnel-broker/
// gooseadapter, for the same reason: Agent isn't something goose's DI can
// zero-value-construct (it needs config, an HTTP client, a Driver, etc.),
// so it's registered as a singleton via Configure rather than declared.
type Module struct {
	agent *core.Agent
}

func NewModule(agent *core.Agent) *Module {
	return &Module{agent: agent}
}

func (m *Module) Imports() []types.Module {
	return []types.Module{
		router.ForRoutes(
			router.Get("/healthz", []any{Controller{}, "Healthz"}),
			router.Get("/readyz", []any{Controller{}, "Readyz"}),
			router.Get("/metrics", []any{Controller{}, "Metrics"}),
		),
	}
}

func (m *Module) Exports() []any      { return []any{} }
func (m *Module) Declarations() []any { return []any{} }

func (m *Module) Configure(container types.Container) error {
	return container.Register(func() *core.Agent { return m.agent }, "", true)
}

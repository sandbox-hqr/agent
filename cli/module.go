package cli

import "github.com/awesome-goose/goose/types"

// Module is the CLI instance's root goose Module.
type Module struct{}

func (m *Module) Imports() []types.Module { return []types.Module{Routes} }
func (m *Module) Exports() []any          { return []any{} }
func (m *Module) Declarations() []any     { return []any{} }

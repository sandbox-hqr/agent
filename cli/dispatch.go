// Package cli is the agent binary's single entrypoint (Run). `agent cli
// <subcommand>` dispatches through goose's own CLI platform, matching
// /Users/isaiahiroko/Projects/awesome-goose/sandbox/cli's conventions;
// anything else (bare `agent`, or `agent run`) runs the daemon, wrapped via
// kardianos/service for correct OS service-manager integration (notably
// required on Windows, where a service must speak the SCM protocol, not
// just run as a bare subprocess).
package cli

import (
	"fmt"
	"os"

	"github.com/awesome-goose/goose"
	cliplatform "github.com/awesome-goose/goose/platforms/cli"
	"github.com/awesome-goose/goose/types"
	kservice "github.com/kardianos/service"
)

// Run is main()'s entire body.
func Run() {
	if len(os.Args) > 1 && os.Args[1] == "cli" {
		// Strip "cli" before goose's CLI platform builds its own Request
		// from os.Args — otherwise it leaks through as a literal path
		// segment and every route has to be registered under a "cli/"
		// prefix to match. Confirmed by direct reproduction; see
		// ~/Projects/awesome-goose/goose/BUGS.md #3 for the full
		// explanation. Stripping it here instead keeps routes as clean as
		// the reference sandbox/cli example's ("install", not
		// "cli/install").
		os.Args = append([]string{os.Args[0]}, os.Args[2:]...)
		runCLI()
		return
	}
	runService()
}

func runCLI() {
	platform := cliplatform.NewPlatform(cliplatform.WithName("sandbox-agent"))
	initializers := []func(types.Container) error{logInitializer()}

	stop, err := goose.Start(goose.CLI(platform, &Module{}, initializers))
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	_ = stop()

	// goose's CLI Response.Write ignores the handler's returned
	// Output.Code()/WithExitCode() entirely — the process would otherwise
	// always exit 0 regardless of what a command actually returned.
	// Confirmed by direct reproduction; see BUGS.md #4. exitCode
	// (controller.go) is the workaround: controllers set it before
	// returning their Output, and this is the only place that value is
	// ever actually turned into a real process exit status.
	os.Exit(exitCode)
}

func runService() {
	prg := newProgram()
	svc, err := kservice.New(prg, serviceConfig())
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if err := svc.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

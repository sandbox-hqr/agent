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
	kservice "github.com/kardianos/service"
)

// Run is main()'s entire body.
func Run() {
	if len(os.Args) > 1 && os.Args[1] == "cli" {
		// Strip "cli" before goose's CLI platform builds its own Request
		// from os.Args — otherwise it leaks through as a literal path
		// segment and every route has to be registered under a "cli/"
		// prefix to match. This is the same underlying mechanism BUGS.md #3
		// describes (an unstripped "cli" selector leaking into route
		// matching) — but that bug was specifically about goose's own
		// runMulti/runCLI internal dispatch (combining goose.API + goose.CLI
		// in one goose.Start call), which is fixed as of v0.0.13 and doesn't
		// apply here regardless: this app's own Run() does its own cli-vs-
		// daemon dispatch *before* ever calling goose.Start (always
		// CLI-only, via runSingle, which BUGS.md #3 confirms was never
		// affected). So this stripping stays — it's needed because this
		// app's own dispatch consumes os.Args[1], not because of the fixed
		// framework bug.
		os.Args = append([]string{os.Args[0]}, os.Args[2:]...)
		runCLI()
		return
	}
	runService()
}

func runCLI() {
	platform := cliplatform.NewPlatform(cliplatform.WithName("sandbox-agent"))

	// A custom Log initializer used to be required here too (goose's own
	// default registration bound the wrong type, BUGS.md #1) — fixed as of
	// goose v0.0.13.
	stop, err := goose.Start(goose.CLI(platform, &Module{}, nil))
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	_ = stop()

	// goose's CLI Response.Write used to ignore the handler's returned
	// Output.Code()/WithExitCode() entirely — the process always exited 0
	// regardless of what a command actually returned (BUGS.md #4). Fixed
	// as of goose v0.0.13: App.Run now calls os.Exit(code) itself for any
	// nonzero code (before goose.Start even returns here), so the
	// package-level exitCode var this used to read is gone — nothing left
	// to do after stop() for the success path.
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

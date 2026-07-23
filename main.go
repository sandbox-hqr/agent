// Command agent is the PoolMesh/sandbox binary that runs on every host
// contributed to the pool (draft/micro-machine.md). `agent` (or `agent
// run`) runs the daemon as an OS service; `agent cli <subcommand>` manages
// it (install/uninstall/start/stop/status) via goose's own CLI platform.
package main

import "github.com/sandbox-hq/agent/cli"

func main() {
	cli.Run()
}

package cli

import "github.com/awesome-goose/goose/modules/router"

// Routes use the same clean, unprefixed convention as the reference
// /Users/isaiahiroko/Projects/awesome-goose/sandbox/cli example — no "cli/"
// prefix needed because dispatch.go's Run() strips the leading "cli"
// argument before goose's CLI platform ever builds its Request from
// os.Args (see that file's comment, and BUGS.md #3).
var Routes = router.ForRoutes(
	router.Cli("install", []any{Controller{}, "Install"}),
	router.Cli("uninstall", []any{Controller{}, "Uninstall"}),
	router.Cli("start", []any{Controller{}, "Start"}),
	router.Cli("stop", []any{Controller{}, "Stop"}),
	router.Cli("status", []any{Controller{}, "Status"}),
)

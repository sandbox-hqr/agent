package cli

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/awesome-goose/goose/io/output"
	"github.com/awesome-goose/goose/types"
	kservice "github.com/kardianos/service"

	"github.com/sandbox-hqr/agent/core"
)

type EmptyDto struct{}

type InstallDto struct {
	Cloud  string `flag:"cloud"`
	Token  string `flag:"token"`
	Driver string `flag:"driver"`
}

type Controller struct{}

// Install is the operator's one-command install flow (draft/
// micro-machine.md Appendix): persist the join token + control-plane URL,
// then install and start the OS service — systemd/launchd/Windows
// Service, same binary either way (kardianos/service picks the backend).
func (c *Controller) Install(dto *InstallDto) types.Output {
	if dto.Cloud == "" || dto.Token == "" {
		return fail("--cloud and --token are required")
	}
	driverType := dto.Driver
	if driverType == "" {
		driverType = "mock"
	}

	if err := os.MkdirAll(core.EnvFilePathDir(), 0700); err != nil {
		return fail(fmt.Sprintf("create state dir: %v", err))
	}
	envContents := fmt.Sprintf(
		"SANDBOX_AGENT_CONTROL_PLANE_URL=%s\nSANDBOX_AGENT_JOIN_TOKEN=%s\nSANDBOX_AGENT_DRIVER=%s\n",
		dto.Cloud, dto.Token, driverType,
	)
	if err := os.WriteFile(core.EnvFilePath(), []byte(envContents), 0600); err != nil {
		return fail(fmt.Sprintf("write env file: %v", err))
	}

	svc, err := kservice.New(newProgram(), serviceConfig())
	if err != nil {
		return fail(fmt.Sprintf("service setup: %v", err))
	}
	if err := kservice.Control(svc, "install"); err != nil {
		return fail(fmt.Sprintf("service install: %v", err))
	}
	if err := kservice.Control(svc, "start"); err != nil {
		return fail(fmt.Sprintf("service start: %v", err))
	}

	return output.ConsoleSuccess("installed and started sandbox-agent")
}

func (c *Controller) Uninstall(_ *EmptyDto) types.Output {
	svc, err := kservice.New(newProgram(), serviceConfig())
	if err != nil {
		return fail(err.Error())
	}
	_ = kservice.Control(svc, "stop")
	if err := kservice.Control(svc, "uninstall"); err != nil {
		return fail(err.Error())
	}
	return output.ConsoleSuccess("uninstalled sandbox-agent")
}

func (c *Controller) Start(_ *EmptyDto) types.Output {
	return c.control("start")
}

func (c *Controller) Stop(_ *EmptyDto) types.Output {
	return c.control("stop")
}

func (c *Controller) control(action string) types.Output {
	svc, err := kservice.New(newProgram(), serviceConfig())
	if err != nil {
		return fail(err.Error())
	}
	if err := kservice.Control(svc, action); err != nil {
		return fail(err.Error())
	}
	return output.ConsoleSuccess(fmt.Sprintf("%sed sandbox-agent", action))
}

// Status queries the daemon's own local health API rather than just the
// OS service manager's process-alive bit, so it also reports
// enrollment/tunnel state (draft/micro-machine.md §9).
func (c *Controller) Status(_ *EmptyDto) types.Output {
	cfg := core.LoadConfig()
	client := &http.Client{Timeout: 3 * time.Second}

	resp, err := client.Get("http://" + cfg.LocalAPIBindAddr + "/readyz")
	if err != nil {
		return fail("unreachable (is the service running?)")
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	box := output.Box("sandbox-agent status", []string{
		fmt.Sprintf("HTTP %d", resp.StatusCode),
		string(body),
	})
	if resp.StatusCode != http.StatusOK {
		return box.WithExitCode(1)
	}
	return box
}

func fail(message string) types.Output {
	return output.ConsoleError(message).WithExitCode(1)
}

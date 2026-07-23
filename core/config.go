package core

import (
	"bufio"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Config is the agent's configuration surface — draft/micro-machine.md
// §10's table, with the POOLMESH_* env var prefix renamed to
// SANDBOX_AGENT_* (SANDBOX.md §7).
type Config struct {
	ControlPlaneBaseURL string
	JoinToken           string
	HeartbeatInterval   time.Duration
	LongpollTimeout     time.Duration
	RetryBackoffMin     time.Duration
	RetryBackoffMax     time.Duration
	RTBBaseURL          string // defaults to ControlPlaneBaseURL — path-routed, same "cloud" endpoint
	DriverType          string // firecracker | cloud-hypervisor | mock
	Labels              map[string]string
	LocalAPIBindAddr    string
	LocalAPIEnablePprof bool
	StateDir            string
}

// EnvFilePath is where `agent cli install` persists the control-plane URL
// and join token, and where LoadConfig reads them back from on every
// daemon start. A plain KEY=VALUE file rather than relying solely on the
// OS service manager's own env-var mechanism (systemd's EnvironmentFile=,
// launchd's EnvironmentVariables dict) because Windows services have no
// equivalent convention — one mechanism that works identically on all
// three platforms beats three different ones.
//
// Reads SANDBOX_AGENT_STATE_DIR directly (not via LoadConfig/loadEnvFile,
// which would be circular — this *is* what locates the env file) so an
// operator overriding the state dir for a non-default install (or a test
// harness using a non-privileged temp dir instead of the real
// /var/lib/sandbox-agent, which needs root to create) affects the env
// file's location too, not just StatePath() as before this fix.
func EnvFilePath() string {
	return EnvFilePathDir() + string(os.PathSeparator) + "agent.env"
}

// EnvFilePathDir is the directory `agent cli install` must create before
// writing EnvFilePath().
func EnvFilePathDir() string {
	return envString("SANDBOX_AGENT_STATE_DIR", defaultStateDir())
}

// loadEnvFile applies KEY=VALUE lines from EnvFilePath() via os.Setenv,
// without overwriting any var already set in the process environment —
// letting an operator override via real env vars for one-off debugging.
func loadEnvFile() {
	f, err := os.Open(EnvFilePath())
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if os.Getenv(key) == "" {
			_ = os.Setenv(key, strings.TrimSpace(value))
		}
	}
}

func LoadConfig() *Config {
	loadEnvFile()

	c := &Config{
		ControlPlaneBaseURL: os.Getenv("SANDBOX_AGENT_CONTROL_PLANE_URL"),
		JoinToken:           os.Getenv("SANDBOX_AGENT_JOIN_TOKEN"),
		HeartbeatInterval:   envDuration("SANDBOX_AGENT_HEARTBEAT_INTERVAL", 15*time.Second),
		LongpollTimeout:     envDuration("SANDBOX_AGENT_LONGPOLL_TIMEOUT", 30*time.Second),
		RetryBackoffMin:     envDuration("SANDBOX_AGENT_RETRY_BACKOFF_MIN", 1*time.Second),
		RetryBackoffMax:     envDuration("SANDBOX_AGENT_RETRY_BACKOFF_MAX", 30*time.Second),
		RTBBaseURL:          os.Getenv("SANDBOX_AGENT_RTB_URL"),
		DriverType:          envString("SANDBOX_AGENT_DRIVER", "mock"),
		LocalAPIBindAddr:    envString("SANDBOX_AGENT_LOCAL_API_ADDR", "127.0.0.1:9090"),
		LocalAPIEnablePprof: envString("SANDBOX_AGENT_LOCAL_API_PPROF", "false") == "true",
		StateDir:            envString("SANDBOX_AGENT_STATE_DIR", defaultStateDir()),
	}
	if c.RTBBaseURL == "" {
		c.RTBBaseURL = c.ControlPlaneBaseURL
	}
	return c
}

func (c *Config) StatePath() string {
	return c.StateDir + string(os.PathSeparator) + "agent.json"
}

func defaultStateDir() string {
	switch runtime.GOOS {
	case "windows":
		if v := os.Getenv("ProgramData"); v != "" {
			return v + `\SandboxAgent`
		}
		return `C:\ProgramData\SandboxAgent`
	default:
		return "/var/lib/sandbox-agent"
	}
}

func envString(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	if n, err := strconv.Atoi(v); err == nil {
		return time.Duration(n) * time.Second
	}
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}
	return def
}

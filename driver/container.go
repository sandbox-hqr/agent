package driver

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// Container is the backend for container-type Spaces (draft/
// micro-machine.md §4's VMM-backend seam, extended beyond microVMs): a
// lighter-weight OCI runtime (runc) instead of a full VM, trading
// Firecracker/Cloud Hypervisor's stronger isolation for faster boot and
// lower per-Space overhead.
//
// Talks to a real runc binary: builds an OCI bundle (a busybox rootfs +
// `runc spec`'s generated config.json, patched to run a fixed command),
// then `runc run -d`. No jailer-equivalent hardening beyond runc's own
// namespace/cgroup isolation, and no image-pulling — like Firecracker's
// rootfs, the container always runs the same staged busybox regardless of
// VMSpec.Image (neither driver resolves Image into a real filesystem
// anywhere in this codebase yet; a real registry-pull step is a shared
// follow-up for both, not specific to this driver).
type Container struct {
	RuncPath    string
	BusyboxPath string
	StateDir    string
}

func NewContainer() *Container {
	return &Container{
		RuncPath:    envDefault("SANDBOX_AGENT_RUNC_BIN", "runc"),
		BusyboxPath: envDefault("SANDBOX_AGENT_CONTAINER_BUSYBOX", "/bin/busybox"),
		StateDir:    envDefault("SANDBOX_AGENT_CONTAINER_STATE_DIR", "/var/lib/sandbox-agent/container"),
	}
}

func (c *Container) Name() string { return "container" }

// Supports accepts only Spaces explicitly requesting the "container"
// type. GPU passthrough into a plain container needs a GPU-aware runtime
// (e.g. nvidia-container-runtime) that this driver doesn't implement, so
// GPU requests are rejected too.
func (c *Container) Supports(spec VMSpec) bool {
	return spec.Type == "container" && spec.Resources.GPUs == 0
}

func (c *Container) bundleDir(id string) string {
	return filepath.Join(c.StateDir, id)
}

func (c *Container) Create(ctx context.Context, spec VMSpec) (*Instance, error) {
	bundle := c.bundleDir(spec.SpaceID)
	rootfsBin := filepath.Join(bundle, "rootfs", "bin")
	if err := os.MkdirAll(rootfsBin, 0755); err != nil {
		return nil, fmt.Errorf("container driver: create rootfs: %w", err)
	}
	if err := copyFile(c.BusyboxPath, filepath.Join(rootfsBin, "busybox")); err != nil {
		return nil, fmt.Errorf("container driver: stage busybox: %w", err)
	}
	if err := os.Chmod(filepath.Join(rootfsBin, "busybox"), 0755); err != nil {
		return nil, fmt.Errorf("container driver: chmod busybox: %w", err)
	}

	if out, err := exec.CommandContext(ctx, c.RuncPath, "spec", "-b", bundle).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("container driver: runc spec: %w: %s", err, out)
	}

	configPath := filepath.Join(bundle, "config.json")
	if err := patchOCIConfig(configPath, spec); err != nil {
		return nil, fmt.Errorf("container driver: patch OCI config: %w", err)
	}

	logFile, err := os.Create(filepath.Join(bundle, "container.log"))
	if err != nil {
		return nil, fmt.Errorf("container driver: create log file: %w", err)
	}
	defer logFile.Close()

	pidPath := filepath.Join(bundle, "pid")
	runCmd := exec.CommandContext(ctx, c.RuncPath, "run", "-d", "--pid-file", pidPath, "-b", bundle, spec.SpaceID)
	runCmd.Stdout = logFile
	runCmd.Stderr = logFile
	if err := runCmd.Run(); err != nil {
		return nil, fmt.Errorf("container driver: runc run: %w", err)
	}

	return &Instance{ID: spec.SpaceID}, nil
}

// patchOCIConfig rewrites just the two fields this driver cares about,
// leaving everything else `runc spec` generated (namespaces, mounts,
// capabilities, cgroup path) untouched — round-tripping through
// map[string]any rather than a hand-maintained OCI struct so an unknown
// field never gets silently dropped.
func patchOCIConfig(path string, spec VMSpec) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return err
	}

	proc, ok := cfg["process"].(map[string]any)
	if !ok {
		return fmt.Errorf("unexpected OCI config shape: no process object")
	}
	proc["terminal"] = false
	proc["args"] = []string{
		"/bin/busybox", "sh", "-c",
		fmt.Sprintf("echo sandbox-container-alive space_id=%s; sleep 300", spec.SpaceID),
	}

	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0644)
}

func (c *Container) Delete(ctx context.Context, id string) error {
	// -f: kill immediately if still running, matching Firecracker.Delete's
	// unconditional teardown semantics. Errors ignored — an already-gone
	// container (or one that never started) still needs its bundle
	// directory cleaned up below.
	_ = exec.CommandContext(ctx, c.RuncPath, "delete", "-f", id).Run()
	return os.RemoveAll(c.bundleDir(id))
}

func (c *Container) List(ctx context.Context) ([]InstanceState, error) {
	out, err := exec.CommandContext(ctx, c.RuncPath, "list", "-f", "json").Output()
	if err != nil {
		return nil, fmt.Errorf("container driver: runc list: %w", err)
	}
	var raw []struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	// runc prints a literal `null` (not `[]`) when no containers exist —
	// json.Unmarshal into a slice leaves raw as nil in that case, no error.
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("container driver: parse runc list: %w", err)
	}
	states := make([]InstanceState, 0, len(raw))
	for _, r := range raw {
		states = append(states, InstanceState{ID: r.ID, Status: r.Status})
	}
	return states, nil
}

func (c *Container) Stats(ctx context.Context, id string) (InstanceStats, error) {
	out, err := exec.CommandContext(ctx, c.RuncPath, "state", id).Output()
	if err != nil {
		return InstanceStats{}, fmt.Errorf("container driver: runc state: %w", err)
	}
	var state struct {
		Pid int `json:"pid"`
	}
	if err := json.Unmarshal(out, &state); err != nil {
		return InstanceStats{}, fmt.Errorf("container driver: parse runc state: %w", err)
	}
	if state.Pid == 0 || !processAlive(state.Pid) {
		return InstanceStats{}, fmt.Errorf("container driver: instance %s not running", id)
	}
	return InstanceStats{MemUsedMB: readVmRSSKB(state.Pid) / 1024}, nil
}

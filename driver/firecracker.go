package driver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Firecracker is the default backend for CPU workloads (draft/
// micro-machine.md §4): ~125ms boot, tiny memory overhead, strong
// isolation via jailer + seccomp + cgroups.
//
// Talks to a real firecracker binary over its REST-over-unix-socket API
// (boot-source / drives / machine-config / actions) — the same wire
// protocol Firecracker's own quickstart guide describes. Deliberately not
// implemented: the jailer (chroot/cgroup/seccomp sandboxing — a hardening
// layer, not required to prove the VMM integration itself works),
// per-tenant tap/subnet networking (draft/micro-machine.md §5, not wired
// up anywhere else in this codebase yet either), and a real copy-on-write
// rootfs overlay (the template rootfs is flat-copied per instance
// instead). None of that changes where this fails on a host without KVM:
// Create reaches the real InstanceStart call and fails there (or earlier,
// if the firecracker process itself can't open /dev/kvm) — see
// SANDBOX.md §21.
type Firecracker struct {
	BinaryPath string
	KernelPath string
	RootfsPath string
	StateDir   string
}

func NewFirecracker() *Firecracker {
	return &Firecracker{
		BinaryPath: envDefault("SANDBOX_AGENT_FIRECRACKER_BIN", "firecracker"),
		KernelPath: os.Getenv("SANDBOX_AGENT_FIRECRACKER_KERNEL"),
		RootfsPath: os.Getenv("SANDBOX_AGENT_FIRECRACKER_ROOTFS"),
		StateDir:   envDefault("SANDBOX_AGENT_FIRECRACKER_STATE_DIR", "/var/lib/sandbox-agent/firecracker"),
	}
}

func (f *Firecracker) Name() string { return "firecracker" }

// Supports returns false whenever GPUs are requested — Firecracker has no
// PCI passthrough, a hard hardware constraint, not a policy choice
// (draft/micro-machine.md §4) — and false for spec.Type == "container",
// since Firecracker only ever boots a microVM.
func (f *Firecracker) Supports(spec VMSpec) bool {
	return spec.Resources.GPUs == 0 && spec.Type != "container"
}

func (f *Firecracker) instanceDir(id string) string {
	return filepath.Join(f.StateDir, id)
}

// Create spawns a real firecracker process, configures it over its API
// socket, and issues InstanceStart. On a host without /dev/kvm this fails
// at (or just before) that last call — a genuine failure from the real
// VMM, not a stubbed-out error.
func (f *Firecracker) Create(ctx context.Context, spec VMSpec) (*Instance, error) {
	if f.KernelPath == "" || f.RootfsPath == "" {
		return nil, fmt.Errorf("firecracker driver: SANDBOX_AGENT_FIRECRACKER_KERNEL and SANDBOX_AGENT_FIRECRACKER_ROOTFS must be set")
	}

	dir := f.instanceDir(spec.SpaceID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("firecracker driver: create instance dir: %w", err)
	}

	rootfsCopy := filepath.Join(dir, "rootfs.ext4")
	if err := copyFile(f.RootfsPath, rootfsCopy); err != nil {
		return nil, fmt.Errorf("firecracker driver: copy rootfs: %w", err)
	}

	sockPath := filepath.Join(dir, "firecracker.sock")
	os.Remove(sockPath) // stale socket from a previous crashed attempt

	logFile, err := os.Create(filepath.Join(dir, "firecracker.log"))
	if err != nil {
		return nil, fmt.Errorf("firecracker driver: create log file: %w", err)
	}
	defer logFile.Close()

	cmd := exec.Command(f.BinaryPath, "--api-sock", sockPath)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("firecracker driver: start firecracker: %w", err)
	}

	cleanup := func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	}

	if err := waitForSocket(sockPath, 3*time.Second); err != nil {
		cleanup()
		return nil, fmt.Errorf("firecracker driver: firecracker did not open its API socket (likely crashed at startup): %w", err)
	}

	client := unixHTTPClient(sockPath)

	vcpus := spec.Resources.VCPUs
	if vcpus < 1 {
		vcpus = 1
	}
	memMB := spec.Resources.MemMB
	if memMB < 128 {
		memMB = 128
	}

	if err := apiPut(ctx, client, "boot-source", map[string]any{
		"kernel_image_path": f.KernelPath,
		"boot_args":         "console=ttyS0 reboot=k panic=1 pci=off",
	}); err != nil {
		cleanup()
		return nil, fmt.Errorf("firecracker driver: configure boot-source: %w", err)
	}

	if err := apiPut(ctx, client, "drives/rootfs", map[string]any{
		"drive_id":       "rootfs",
		"path_on_host":   rootfsCopy,
		"is_root_device": true,
		"is_read_only":   false,
	}); err != nil {
		cleanup()
		return nil, fmt.Errorf("firecracker driver: configure rootfs drive: %w", err)
	}

	if err := apiPut(ctx, client, "machine-config", map[string]any{
		"vcpu_count":   vcpus,
		"mem_size_mib": memMB,
	}); err != nil {
		cleanup()
		return nil, fmt.Errorf("firecracker driver: configure machine: %w", err)
	}

	// InstanceStart is where the VMM actually opens /dev/kvm and issues
	// KVM_CREATE_VM — on a host without hardware virtualization exposed,
	// this (or an even earlier step, if firecracker probes /dev/kvm at
	// process init) is what fails. See SANDBOX.md §21.
	if err := apiPut(ctx, client, "actions", map[string]any{
		"action_type": "InstanceStart",
	}); err != nil {
		cleanup()
		return nil, fmt.Errorf("firecracker driver: start instance: %w", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "pid"), []byte(strconv.Itoa(cmd.Process.Pid)), 0600); err != nil {
		cleanup()
		return nil, fmt.Errorf("firecracker driver: persist pid: %w", err)
	}

	return &Instance{ID: spec.SpaceID}, nil
}

func (f *Firecracker) Delete(ctx context.Context, id string) error {
	dir := f.instanceDir(id)
	if pid, ok := f.readPid(id); ok {
		if proc, err := os.FindProcess(pid); err == nil {
			_ = proc.Kill()
			_, _ = proc.Wait()
		}
	}
	return os.RemoveAll(dir)
}

func (f *Firecracker) List(ctx context.Context) ([]InstanceState, error) {
	entries, err := os.ReadDir(f.StateDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var states []InstanceState
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		status := "stopped"
		if pid, ok := f.readPid(e.Name()); ok && processAlive(pid) {
			status = "running"
		}
		states = append(states, InstanceState{ID: e.Name(), Status: status})
	}
	return states, nil
}

func (f *Firecracker) Stats(ctx context.Context, id string) (InstanceStats, error) {
	pid, ok := f.readPid(id)
	if !ok || !processAlive(pid) {
		return InstanceStats{}, fmt.Errorf("firecracker driver: instance %s not running", id)
	}
	return InstanceStats{MemUsedMB: readVmRSSKB(pid) / 1024}, nil
}

func (f *Firecracker) readPid(id string) (int, bool) {
	b, err := os.ReadFile(filepath.Join(f.instanceDir(id), "pid"))
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	return pid, err == nil
}

// --- shared helpers (also usable by any future real Cloud Hypervisor
// implementation, which speaks a near-identical REST-over-unix-socket API) ---

func envDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func waitForSocket(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for %s", path)
}

func unixHTTPClient(sockPath string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sockPath)
			},
		},
		Timeout: 5 * time.Second,
	}
}

func apiPut(ctx context.Context, client *http.Client, path string, body map[string]any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://unix/"+path, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("status %d: %s", resp.StatusCode, bytes.TrimSpace(respBody))
	}
	return nil
}

func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

func readVmRSSKB(pid int) int {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				n, _ := strconv.Atoi(fields[1])
				return n
			}
		}
	}
	return 0
}

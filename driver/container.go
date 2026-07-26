package driver

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Container is the backend for container-type Spaces (draft/
// micro-machine.md §4's VMM-backend seam, extended beyond microVMs): a
// lighter-weight OCI runtime (runc) instead of a full VM, trading
// Firecracker/Cloud Hypervisor's stronger isolation for faster boot and
// lower per-Space overhead.
//
// Talks to a real runc binary: builds an OCI bundle (a busybox+dropbear
// rootfs + `runc spec`'s generated config.json, patched to run dropbear),
// wires it onto a real bridge network via the `bridge` CNI plugin
// (cni.go), then `runc run -d`. No jailer-equivalent hardening beyond
// runc's own namespace/cgroup isolation, and no image-pulling — the
// container always runs the same staged busybox+dropbear regardless of
// VMSpec.Image (neither driver resolves Image into a real filesystem
// anywhere in this codebase yet; a real registry-pull step is a shared
// follow-up for both, not specific to this driver — SPACE_ACCESS.md's
// "related but out of scope" section).
//
// Real per-Space networking (SPACE_ACCESS.md phase 2) replaces the
// earlier SANDBOX_AGENT_CONTAINER_HOST_NETWORK stopgap (SANDBOX.md §22)
// entirely — that flag shared the *entire* host network namespace; this
// gives every container its own veth+bridge-backed private IP instead.
type Container struct {
	RuncPath           string
	BusyboxPath        string
	DropbearPath       string
	DropbearKeygenPath string
	StateDir           string
	CNIBinDir          string
	BridgeName         string
	Subnet             string
	SSHPort            int
}

func NewContainer() *Container {
	return &Container{
		RuncPath:           envDefault("SANDBOX_AGENT_RUNC_BIN", "runc"),
		BusyboxPath:        envDefault("SANDBOX_AGENT_CONTAINER_BUSYBOX", "/bin/busybox"),
		DropbearPath:       envDefault("SANDBOX_AGENT_CONTAINER_DROPBEAR", "/usr/sbin/dropbear"),
		DropbearKeygenPath: envDefault("SANDBOX_AGENT_CONTAINER_DROPBEARKEY", "/usr/bin/dropbearkey"),
		StateDir:           envDefault("SANDBOX_AGENT_CONTAINER_STATE_DIR", "/var/lib/sandbox-agent/container"),
		CNIBinDir:          envDefault("SANDBOX_AGENT_CNI_BIN_DIR", "/usr/lib/cni"),
		BridgeName:         envDefault("SANDBOX_AGENT_CONTAINER_BRIDGE", "sandbox0"),
		Subnet:             envDefault("SANDBOX_AGENT_CONTAINER_SUBNET", "10.42.0.0/24"),
		SSHPort:            envIntDefault("SANDBOX_AGENT_CONTAINER_SSH_PORT", 2222),
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

func (c *Container) pidFile(id string) string {
	return filepath.Join(c.bundleDir(id), "pid")
}

func (c *Container) Create(ctx context.Context, spec VMSpec) (*Instance, error) {
	bundle := c.bundleDir(spec.SpaceID)

	// A retry of a failed Create (redelivered command, agent/core/
	// agent.go never marks a failed create_space as executed) must start
	// from a clean bundle dir — `runc spec` refuses to overwrite an
	// existing config.json, and stale state from a partial previous
	// attempt could otherwise wire up the wrong thing. success is only
	// set true right before the final return.
	success := false
	defer func() {
		if !success {
			_ = exec.Command(c.RuncPath, "delete", "-f", spec.SpaceID).Run()
			_ = os.RemoveAll(bundle)
		}
	}()

	rootfs := filepath.Join(bundle, "rootfs")
	rootfsBin := filepath.Join(rootfs, "bin")
	if err := os.MkdirAll(rootfsBin, 0755); err != nil {
		return nil, fmt.Errorf("container driver: create rootfs: %w", err)
	}
	if err := copyFile(c.BusyboxPath, filepath.Join(rootfsBin, "busybox")); err != nil {
		return nil, fmt.Errorf("container driver: stage busybox: %w", err)
	}
	if err := os.Chmod(filepath.Join(rootfsBin, "busybox"), 0755); err != nil {
		return nil, fmt.Errorf("container driver: chmod busybox: %w", err)
	}
	// busybox provides a POSIX shell when invoked as "sh" — this is what
	// a connecting SSH session (or anything else needing /bin/sh) execs.
	if err := os.Symlink("busybox", filepath.Join(rootfsBin, "sh")); err != nil && !os.IsExist(err) {
		return nil, fmt.Errorf("container driver: symlink /bin/sh: %w", err)
	}

	sshEnabled := spec.SSHPublicKey != ""
	if sshEnabled {
		if err := c.stageDropbear(rootfs, spec.SSHPublicKey); err != nil {
			return nil, fmt.Errorf("container driver: stage dropbear: %w", err)
		}
	}

	if out, err := exec.CommandContext(ctx, c.RuncPath, "spec", "-b", bundle).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("container driver: runc spec: %w: %s", err, out)
	}

	configPath := filepath.Join(bundle, "config.json")
	if err := c.patchOCIConfig(configPath, sshEnabled); err != nil {
		return nil, fmt.Errorf("container driver: patch OCI config: %w", err)
	}

	logFile, err := os.Create(filepath.Join(bundle, "container.log"))
	if err != nil {
		return nil, fmt.Errorf("container driver: create log file: %w", err)
	}
	defer logFile.Close()

	runCmd := exec.CommandContext(ctx, c.RuncPath, "run", "-d", "--pid-file", c.pidFile(spec.SpaceID), "-b", bundle, spec.SpaceID)
	runCmd.Stdout = logFile
	runCmd.Stderr = logFile
	if err := runCmd.Run(); err != nil {
		return nil, fmt.Errorf("container driver: runc run: %w", err)
	}

	pid, ok := c.readPid(spec.SpaceID)
	if !ok {
		return nil, fmt.Errorf("container driver: read pid after start: pid file missing or unparseable")
	}

	// The container's netns exists as soon as runc creates the container
	// (well before runc run -d returns), so wiring it here — after the
	// process may already be running — is safe: an app that binds
	// 0.0.0.0 before eth0 has an address still ends up listening on it
	// once cniAdd assigns one, no restart needed. See cni.go's doc
	// comment for why a true OCI-hook-based integration (network ready
	// strictly before the container's first instruction) wasn't used
	// here instead.
	ip, err := cniAdd(ctx, c.CNIBinDir, c.BridgeName, c.Subnet, spec.SpaceID, pid)
	if err != nil {
		return nil, fmt.Errorf("container driver: cni add: %w", err)
	}

	success = true
	return &Instance{ID: spec.SpaceID, IP: ip}, nil
}

// stageDropbear copies the dropbear binary plus its full shared-library
// closure (resolved via the host's own `ldd` — dropbear, unlike busybox,
// isn't statically linked) into the rootfs, generates a per-Space
// ed25519 host key on the host side (no netns/chroot needed for
// keygen — it just writes a file), and injects the caller's public key
// into /root/.ssh/authorized_keys. Host keys and injected key live under
// the bundle dir, so a redelivered create_space command (idempotent per
// agent/core/agent.go, but a genuinely new command after a Node restart
// would call this again) reuses the same host key instead of generating
// a new one and training the connecting user to ignore a host-key-
// changed warning.
func (c *Container) stageDropbear(rootfs, sshPublicKey string) error {
	for _, lib := range mustLddDeps(c.DropbearPath) {
		if err := stageAbsoluteFile(rootfs, lib); err != nil {
			return fmt.Errorf("stage dropbear dependency %s: %w", lib, err)
		}
	}
	if err := stageAbsoluteFile(rootfs, c.DropbearPath); err != nil {
		return fmt.Errorf("stage dropbear binary: %w", err)
	}
	if err := os.Chmod(filepath.Join(rootfs, c.DropbearPath), 0755); err != nil {
		return err
	}

	// Minimal /etc/passwd + /etc/group — dropbear needs these to resolve
	// the connecting user (root) to a home directory and login shell.
	etc := filepath.Join(rootfs, "etc")
	if err := os.MkdirAll(etc, 0755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(etc, "passwd"), []byte("root:x:0:0:root:/root:/bin/sh\n"), 0644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(etc, "group"), []byte("root:x:0:\n"), 0644); err != nil {
		return err
	}

	dropbearDir := filepath.Join(etc, "dropbear")
	if err := os.MkdirAll(dropbearDir, 0700); err != nil {
		return err
	}
	hostKeyPath := filepath.Join(dropbearDir, "dropbear_ed25519_host_key")
	if _, err := os.Stat(hostKeyPath); os.IsNotExist(err) {
		if out, err := exec.Command(c.DropbearKeygenPath, "-t", "ed25519", "-f", hostKeyPath).CombinedOutput(); err != nil {
			return fmt.Errorf("generate host key: %w: %s", err, out)
		}
	}

	sshDir := filepath.Join(rootfs, "root", ".ssh")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(sshDir, "authorized_keys"), []byte(sshPublicKey+"\n"), 0600); err != nil {
		return err
	}
	return nil
}

func mustLddDeps(binPath string) []string {
	libs, err := lddDeps(binPath)
	if err != nil {
		return nil
	}
	return libs
}

// stageAbsoluteFile copies a host file into rootfs at the identical
// absolute path (e.g. host /lib/x86_64-linux-gnu/libc.so.6 -> rootfs
// /lib/x86_64-linux-gnu/libc.so.6) — dropbear's dynamic linker resolves
// its dependencies by that same absolute path inside the container's own
// mount namespace, so paths have to match exactly, not just be present
// somewhere. Preserves the source file's permission bits (copyFile's
// os.Create destination otherwise defaults to a plain 0666-minus-umask,
// which drops the execute bit the ELF interpreter — ld-linux-x86-64.so.2
// itself, invoked directly by the kernel's loader, not just mmap'd like
// an ordinary shared library — needs to be exec'd at all).
func stageAbsoluteFile(rootfs, hostPath string) error {
	info, err := os.Stat(hostPath)
	if err != nil {
		return err
	}
	dest := filepath.Join(rootfs, hostPath)
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return err
	}
	if err := copyFile(hostPath, dest); err != nil {
		return err
	}
	return os.Chmod(dest, info.Mode().Perm())
}

// patchOCIConfig rewrites just the fields this driver cares about,
// leaving everything else `runc spec` generated (namespaces, mounts,
// capabilities, cgroup path) untouched — round-tripping through
// map[string]any rather than a hand-maintained OCI struct so an unknown
// field never gets silently dropped.
func (c *Container) patchOCIConfig(path string, sshEnabled bool) error {
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
	if sshEnabled {
		proc["args"] = []string{
			c.DropbearPath, "-F", "-E", "-R", "-p", strconv.Itoa(c.SSHPort),
		}
		// dropbear re-asserts its session's gid/uid (setresgid/setgroups)
		// even when the connecting user is already root — Linux gates
		// those syscalls on capabilities, not UID, so runc spec's default
		// minimal set (CAP_AUDIT_WRITE/CAP_KILL/CAP_NET_BIND_SERVICE) makes
		// that step fail with "Error changing user group" even though
		// nothing is actually trying to change to a different id. Found
		// live: pubkey auth succeeded, then dropbear tore the session back
		// down at exactly this step.
		if caps, ok := proc["capabilities"].(map[string]any); ok {
			for _, set := range []string{"bounding", "effective", "permitted"} {
				addCapability(caps, set, "CAP_SETGID")
				addCapability(caps, set, "CAP_SETUID")
			}
		}
	} else {
		// No key supplied: stay alive and networked, but don't start a
		// keyless/passwordless SSH server (SPACE_ACCESS.md phase 3).
		proc["args"] = []string{"/bin/busybox", "sh", "-c", "while true; do sleep 3600; done"}
	}

	// runc spec's default root is read-only; dropbear needs to write its
	// pid file and (if -R ever actually has to generate a key at runtime,
	// belt-and-suspenders alongside the pre-generated one above) host
	// key files under /etc/dropbear.
	if root, ok := cfg["root"].(map[string]any); ok {
		root["readonly"] = false
	}

	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0644)
}

// addCapability appends a capability to one of an OCI process spec's
// capability sets if not already present.
func addCapability(caps map[string]any, set, capability string) {
	list, _ := caps[set].([]any)
	for _, c := range list {
		if c == capability {
			return
		}
	}
	caps[set] = append(list, capability)
}

func (c *Container) readPid(id string) (int, bool) {
	b, err := os.ReadFile(c.pidFile(id))
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	return pid, err == nil
}

func (c *Container) Delete(ctx context.Context, id string) error {
	// cniDel needs the container's netns to still resolve, so it runs
	// before runc delete tears the container (and its netns) down —
	// same ordering containerd/CRI-O use. Best-effort: an already-gone
	// container (crashed, or this is a retry of a previous partial
	// delete) still needs its bundle directory cleaned up regardless.
	if pid, ok := c.readPid(id); ok {
		_ = cniDel(ctx, c.CNIBinDir, c.BridgeName, c.Subnet, id, pid)
	}
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

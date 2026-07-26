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
// (boot-source / drives / network-interfaces / machine-config / actions)
// — the same wire protocol Firecracker's own quickstart guide describes.
// Deliberately not implemented: the jailer (chroot/cgroup/seccomp
// sandboxing — a hardening layer, not required to prove the VMM
// integration itself works), key injection into the guest image
// (SPACE_ACCESS.md phase 6), and a real copy-on-write rootfs overlay (the
// template rootfs is flat-copied per instance instead). None of that
// changes where this fails on a host without KVM: Create reaches the real
// InstanceStart call and fails there (or earlier, if the firecracker
// process itself can't open /dev/kvm) — see SANDBOX.md §21.
//
// Real per-Space networking (SPACE_ACCESS.md phase 5): a tap device per
// VM, attached to a dedicated bridge, with the guest's IP handed to it via
// the kernel's own `ip=` boot parameter (no DHCP client or cloud-init
// needed) — the VM-side counterpart to the container driver's veth+bridge
// setup (cni.go). Everything up through the network-interfaces API call
// is verified the same way boot-source/drives/machine-config already are
// (SANDBOX.md §21): correct driver behavior can be proven without KVM;
// whether the guest's eth0 actually comes up cannot be, without a
// KVM-capable host to boot it on.
type Firecracker struct {
	BinaryPath      string
	KernelPath      string
	RootfsPath      string
	StateDir        string
	CNIBinDir       string
	BridgeName      string
	Subnet          string
	IPAMNetworkName string
}

func NewFirecracker() *Firecracker {
	return &Firecracker{
		BinaryPath:      envDefault("SANDBOX_AGENT_FIRECRACKER_BIN", "firecracker"),
		KernelPath:      os.Getenv("SANDBOX_AGENT_FIRECRACKER_KERNEL"),
		RootfsPath:      os.Getenv("SANDBOX_AGENT_FIRECRACKER_ROOTFS"),
		StateDir:        envDefault("SANDBOX_AGENT_FIRECRACKER_STATE_DIR", "/var/lib/sandbox-agent/firecracker"),
		CNIBinDir:       envDefault("SANDBOX_AGENT_CNI_BIN_DIR", "/usr/lib/cni"),
		BridgeName:      envDefault("SANDBOX_AGENT_FIRECRACKER_BRIDGE", "sandbox-vm0"),
		Subnet:          envDefault("SANDBOX_AGENT_FIRECRACKER_SUBNET", "10.43.0.0/24"),
		IPAMNetworkName: envDefault("SANDBOX_AGENT_FIRECRACKER_IPAM_NAME", "sandbox-vm0"),
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

	if spec.SSHPublicKey != "" {
		// sshd + its host keys are already present in Firecracker's own
		// quickstart rootfs image (confirmed by inspection, not assumed —
		// SPACE_ACCESS.md phase 6's "does the guest already have sshd"
		// open question) — this is purely key injection, no server
		// bootstrap needed, unlike the container path's dropbear staging.
		if err := injectSSHKey(ctx, rootfsCopy, spec.SSHPublicKey); err != nil {
			return nil, fmt.Errorf("firecracker driver: inject ssh key: %w", err)
		}
	}

	if err := f.ensureBridge(ctx); err != nil {
		return nil, fmt.Errorf("firecracker driver: ensure bridge: %w", err)
	}

	tapName := tapDeviceName(spec.SpaceID)
	if err := setupTapDevice(ctx, tapName, f.BridgeName); err != nil {
		return nil, fmt.Errorf("firecracker driver: create tap device: %w", err)
	}

	vmAddr, err := vmIPAMAlloc(ctx, f.CNIBinDir, f.IPAMNetworkName, f.Subnet, spec.SpaceID)
	if err != nil {
		removeTapDevice(tapName)
		return nil, fmt.Errorf("firecracker driver: allocate VM address: %w", err)
	}
	vmIP, ipNet, err := net.ParseCIDR(vmAddr)
	if err != nil {
		removeTapDevice(tapName)
		_ = vmIPAMRelease(ctx, f.CNIBinDir, f.IPAMNetworkName, f.Subnet, spec.SpaceID)
		return nil, fmt.Errorf("firecracker driver: parse allocated address %q: %w", vmAddr, err)
	}
	gateway, err := gatewayForSubnet(f.Subnet)
	if err != nil {
		removeTapDevice(tapName)
		_ = vmIPAMRelease(ctx, f.CNIBinDir, f.IPAMNetworkName, f.Subnet, spec.SpaceID)
		return nil, fmt.Errorf("firecracker driver: compute gateway: %w", err)
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
		removeTapDevice(tapName)
		_ = vmIPAMRelease(ctx, f.CNIBinDir, f.IPAMNetworkName, f.Subnet, spec.SpaceID)
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

	prefixLen, _ := ipNet.Mask.Size()
	netmask := netmaskDotted(prefixLen)
	// Static guest network config via the kernel's own early-boot "ip="
	// parameter — no DHCP client or cloud-init needed in the guest.
	bootArgs := fmt.Sprintf(
		"console=ttyS0 reboot=k panic=1 pci=off ip=%s::%s:%s::eth0:off",
		vmIP.String(), gateway, netmask,
	)
	if err := apiPut(ctx, client, "boot-source", map[string]any{
		"kernel_image_path": f.KernelPath,
		"boot_args":         bootArgs,
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

	if err := apiPut(ctx, client, "network-interfaces/eth0", map[string]any{
		"iface_id":      "eth0",
		"host_dev_name": tapName,
	}); err != nil {
		cleanup()
		return nil, fmt.Errorf("firecracker driver: configure network interface: %w", err)
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

	return &Instance{ID: spec.SpaceID, IP: vmIP.String()}, nil
}

func (f *Firecracker) Delete(ctx context.Context, id string) error {
	dir := f.instanceDir(id)
	if pid, ok := f.readPid(id); ok {
		if proc, err := os.FindProcess(pid); err == nil {
			_ = proc.Kill()
			_, _ = proc.Wait()
		}
	}
	removeTapDevice(tapDeviceName(id))
	_ = vmIPAMRelease(ctx, f.CNIBinDir, f.IPAMNetworkName, f.Subnet, id)
	return os.RemoveAll(dir)
}

// injectSSHKey loop-mounts a (per-instance, already-copied — never the
// shared template) rootfs image read-write, appends the caller's public
// key to /root/.ssh/authorized_keys, and unmounts — the standard
// Firecracker pattern for seeding a guest before boot, since there's no
// running guest yet to inject anything into any other way. Always
// unmounts on every path out, even on a write failure, so a stuck loop
// mount never leaks past this one call.
func injectSSHKey(ctx context.Context, rootfsPath, publicKey string) error {
	mountpoint, err := os.MkdirTemp(filepath.Dir(rootfsPath), "mnt-")
	if err != nil {
		return fmt.Errorf("create mountpoint: %w", err)
	}
	defer os.Remove(mountpoint)

	if out, err := exec.CommandContext(ctx, "mount", "-o", "loop", rootfsPath, mountpoint).CombinedOutput(); err != nil {
		return fmt.Errorf("mount %s: %w: %s", rootfsPath, err, out)
	}
	defer func() {
		_ = exec.Command("umount", mountpoint).Run()
	}()

	sshDir := filepath.Join(mountpoint, "root", ".ssh")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		return fmt.Errorf("create %s: %w", sshDir, err)
	}
	authorizedKeys := filepath.Join(sshDir, "authorized_keys")
	f, err := os.OpenFile(authorizedKeys, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("open %s: %w", authorizedKeys, err)
	}
	defer f.Close()
	if _, err := f.WriteString(publicKey + "\n"); err != nil {
		return fmt.Errorf("write %s: %w", authorizedKeys, err)
	}
	return nil
}

// ensureBridge idempotently creates the host bridge every VM's tap device
// attaches to, and gives it a gateway IP (the first address in Subnet) —
// mirroring what the container path's `bridge` CNI plugin does
// automatically via isGateway:true, but done by hand here since tap
// devices for a VM don't go through that plugin (no netns to hand it).
// "already exists"-shaped errors from either `ip` invocation are expected
// on every Create after the first and are intentionally not treated as
// failures.
func (f *Firecracker) ensureBridge(ctx context.Context) error {
	_ = exec.CommandContext(ctx, "ip", "link", "add", f.BridgeName, "type", "bridge").Run()
	if err := exec.CommandContext(ctx, "ip", "link", "set", f.BridgeName, "up").Run(); err != nil {
		return fmt.Errorf("bring up bridge %s: %w", f.BridgeName, err)
	}
	gateway, err := gatewayForSubnet(f.Subnet)
	if err != nil {
		return err
	}
	_, ipNet, err := net.ParseCIDR(f.Subnet)
	if err != nil {
		return fmt.Errorf("parse subnet %q: %w", f.Subnet, err)
	}
	prefixLen, _ := ipNet.Mask.Size()
	_ = exec.CommandContext(ctx, "ip", "addr", "add", fmt.Sprintf("%s/%d", gateway, prefixLen), "dev", f.BridgeName).Run()
	return nil
}

// tapDeviceName derives a host interface name from a Space ID — Linux
// interface names are capped at 15 bytes (IFNAMSIZ-1), far shorter than a
// UUID, so this uses just enough of it (8 hex chars, with dashes
// stripped) to stay unique in practice for however many VMs one Node
// actually runs concurrently.
func tapDeviceName(spaceID string) string {
	compact := strings.ReplaceAll(spaceID, "-", "")
	if len(compact) > 8 {
		compact = compact[:8]
	}
	return "tap-" + compact
}

func setupTapDevice(ctx context.Context, tapName, bridgeName string) error {
	if err := exec.CommandContext(ctx, "ip", "tuntap", "add", "dev", tapName, "mode", "tap").Run(); err != nil {
		return fmt.Errorf("create tap %s: %w", tapName, err)
	}
	if err := exec.CommandContext(ctx, "ip", "link", "set", tapName, "master", bridgeName).Run(); err != nil {
		removeTapDevice(tapName)
		return fmt.Errorf("attach tap %s to bridge %s: %w", tapName, bridgeName, err)
	}
	if err := exec.CommandContext(ctx, "ip", "link", "set", tapName, "up").Run(); err != nil {
		removeTapDevice(tapName)
		return fmt.Errorf("bring up tap %s: %w", tapName, err)
	}
	return nil
}

// removeTapDevice is best-effort cleanup — called from failure paths and
// Delete alike, where a tap device that's already gone (or never got
// created) isn't itself an error worth surfacing.
func removeTapDevice(tapName string) {
	_ = exec.Command("ip", "link", "del", tapName).Run()
}

// gatewayForSubnet returns the first host address in subnet (e.g.
// "10.43.0.0/24" -> "10.43.0.1") — assigned to the bridge itself, and
// handed to each VM as its default gateway via the kernel `ip=` boot
// parameter. Only correct for subnets where incrementing the last octet
// of the network address doesn't overflow it, true for this driver's
// fixed /24 default and anything of similar or coarser granularity.
func gatewayForSubnet(subnet string) (string, error) {
	ip, _, err := net.ParseCIDR(subnet)
	if err != nil {
		return "", fmt.Errorf("parse subnet %q: %w", subnet, err)
	}
	v4 := ip.To4()
	if v4 == nil {
		return "", fmt.Errorf("subnet %q is not IPv4", subnet)
	}
	gw := make(net.IP, len(v4))
	copy(gw, v4)
	gw[len(gw)-1]++
	return gw.String(), nil
}

// netmaskDotted converts a CIDR prefix length to dotted-decimal form
// (e.g. 24 -> "255.255.255.0") — the format the kernel's `ip=` boot
// parameter expects, not the /24 shorthand.
func netmaskDotted(prefixLen int) string {
	mask := net.CIDRMask(prefixLen, 32)
	return net.IP(mask).String()
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

func envIntDefault(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
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

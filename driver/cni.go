package driver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
)

// CNI plumbing for real per-Space container networking (SPACE_ACCESS.md
// phase 2) — replaces the earlier SANDBOX_AGENT_CONTAINER_HOST_NETWORK
// stopgap (SANDBOX.md §22) entirely. Invokes the standard `bridge` CNI
// plugin (containernetworking-plugins) directly, the same low-level
// protocol containerd/CRI-O use under the hood: a network config JSON on
// stdin, CNI_* env vars identifying the operation, and (for ADD) a CNI
// Result JSON on stdout. No CNI runtime/daemon involved — this is the
// plugin binary invoked as a plain subprocess.
//
// IP addressing never needs to be unique across Nodes: a Space's traffic
// is always dialed from its own owning Node's agent process
// (agent/core/agent.go's serveStream), never routed between Nodes, so
// every Node can safely reuse the identical bridge/subnet independently
// (SPACE_ACCESS.md phase 2's design note).

type cniResult struct {
	IPs []struct {
		Address string `json:"address"`
	} `json:"ips"`
}

func cniNetworkConfig(bridgeName, subnet string) ([]byte, error) {
	return json.Marshal(map[string]any{
		"cniVersion": "1.0.0",
		"name":       bridgeName,
		"type":       "bridge",
		"bridge":     bridgeName,
		"isGateway":  true,
		"ipMasq":     true,
		"ipam": map[string]any{
			"type":   "host-local",
			"subnet": subnet,
		},
	})
}

// cniExec runs the `bridge` CNI plugin against one container's network
// namespace. command is "ADD" or "DEL"; pid must resolve to a live
// process's /proc/<pid>/ns/net for ADD (the container's netns exists as
// soon as runc creates the container, well before its own process starts
// executing — no race with runc run -d having already returned by the
// time this is called).
func cniExec(ctx context.Context, binDir, bridgeName, subnet, command, containerID string, pid int) ([]byte, error) {
	cfg, err := cniNetworkConfig(bridgeName, subnet)
	if err != nil {
		return nil, err
	}

	cmd := exec.CommandContext(ctx, binDir+"/bridge")
	cmd.Env = append(os.Environ(),
		"CNI_COMMAND="+command,
		"CNI_CONTAINERID="+containerID,
		fmt.Sprintf("CNI_NETNS=/proc/%d/ns/net", pid),
		"CNI_IFNAME=eth0",
		"CNI_PATH="+binDir,
	)
	cmd.Stdin = bytes.NewReader(cfg)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("cni %s: %w: %s", command, err, stderr.String())
	}
	return stdout.Bytes(), nil
}

// cniAdd wires up a container's network namespace (veth pair + bridge,
// auto-created by the plugin if missing) and returns its assigned IP —
// this becomes Instance.IP, which app.hooks.go's existing result hook
// (SANDBOX.md §20) writes onto Space.InternalIP with no changes needed
// on that side.
func cniAdd(ctx context.Context, binDir, bridgeName, subnet, containerID string, pid int) (string, error) {
	out, err := cniExec(ctx, binDir, bridgeName, subnet, "ADD", containerID, pid)
	if err != nil {
		return "", err
	}
	var res cniResult
	if err := json.Unmarshal(out, &res); err != nil {
		return "", fmt.Errorf("cni add: parse result: %w: %s", err, out)
	}
	if len(res.IPs) == 0 {
		return "", fmt.Errorf("cni add: no IP assigned: %s", out)
	}
	ip, _, err := net.ParseCIDR(res.IPs[0].Address)
	if err != nil {
		return "", fmt.Errorf("cni add: parse assigned address %q: %w", res.IPs[0].Address, err)
	}
	return ip.String(), nil
}

// cniDel releases a container's IPAM lease and removes its veth pair —
// called with the container's netns still live (before runc delete),
// matching how containerd/CRI-O sequence teardown, since the plugin
// needs the netns to remove the container-side veth end (the host-side
// end is destroyed automatically once the last reference to its peer's
// netns goes away, but IPAM release depends on this call succeeding
// while the namespace can still be resolved).
func cniDel(ctx context.Context, binDir, bridgeName, subnet, containerID string, pid int) error {
	_, err := cniExec(ctx, binDir, bridgeName, subnet, "DEL", containerID, pid)
	return err
}

// vmIPAM* — Firecracker path (SPACE_ACCESS.md phase 5). A VM doesn't get
// a CNI-managed network namespace/veth the way a container does: a tap
// device is just a host-side interface Firecracker's own VMM opens
// directly (via its network-interfaces API call), no netns involved. Only
// the IP *bookkeeping* is shared — invoking the same `host-local` IPAM
// plugin directly, rather than hand-rolling a second lease tracker
// alongside the one cniAdd/cniDel already use for containers. Given a
// different network name (and normally a different subnet) than the
// container path, its leases never collide with cniAdd/cniDel's.
func vmIPAMConfig(networkName, subnet string) ([]byte, error) {
	return json.Marshal(map[string]any{
		"cniVersion": "1.0.0",
		"name":       networkName,
		"ipam": map[string]any{
			"type":   "host-local",
			"subnet": subnet,
		},
	})
}

func vmIPAMExec(ctx context.Context, binDir, networkName, subnet, command, containerID string) ([]byte, error) {
	cfg, err := vmIPAMConfig(networkName, subnet)
	if err != nil {
		return nil, err
	}

	cmd := exec.CommandContext(ctx, binDir+"/host-local")
	cmd.Env = append(os.Environ(),
		"CNI_COMMAND="+command,
		"CNI_CONTAINERID="+containerID,
		// host-local doesn't create or enter any namespace — it only
		// needs *a* valid path present to satisfy the CNI plugin
		// protocol's required env vars, never dereferences it otherwise.
		"CNI_NETNS=/proc/1/ns/net",
		"CNI_IFNAME=eth0",
		"CNI_PATH="+binDir,
	)
	cmd.Stdin = bytes.NewReader(cfg)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("host-local %s: %w: %s", command, err, stderr.String())
	}
	return stdout.Bytes(), nil
}

// vmIPAMAlloc returns the assigned address in CIDR form (e.g.
// "10.43.0.2/24") — unlike cniAdd's return value, the prefix length is
// kept here: the kernel's `ip=` boot parameter needs the guest's dotted-
// decimal netmask, derived from that same prefix (firecracker.go's
// netmaskForCIDR).
func vmIPAMAlloc(ctx context.Context, binDir, networkName, subnet, containerID string) (string, error) {
	out, err := vmIPAMExec(ctx, binDir, networkName, subnet, "ADD", containerID)
	if err != nil {
		return "", err
	}
	var res cniResult
	if err := json.Unmarshal(out, &res); err != nil {
		return "", fmt.Errorf("host-local add: parse result: %w: %s", err, out)
	}
	if len(res.IPs) == 0 {
		return "", fmt.Errorf("host-local add: no IP assigned: %s", out)
	}
	return res.IPs[0].Address, nil
}

func vmIPAMRelease(ctx context.Context, binDir, networkName, subnet, containerID string) error {
	_, err := vmIPAMExec(ctx, binDir, networkName, subnet, "DEL", containerID)
	return err
}

// lddDeps returns the real, absolute paths of a dynamically-linked
// binary's shared-library closure (via the host's own `ldd`) — used to
// stage dropbear (not statically linked, unlike busybox) into a
// container rootfs without hardcoding distro-specific library paths.
// Virtual entries ldd reports with no backing file (linux-vdso.so.1) are
// silently skipped; the kernel provides those at runtime regardless of
// whether anything is staged for them.
func lddDeps(binPath string) ([]string, error) {
	out, err := exec.Command("ldd", binPath).Output()
	if err != nil {
		return nil, fmt.Errorf("ldd %s: %w", binPath, err)
	}
	var libs []string
	for _, line := range strings.Split(string(out), "\n") {
		path := parseLddLine(line)
		if path == "" {
			continue
		}
		if _, err := os.Stat(path); err == nil {
			libs = append(libs, path)
		}
	}
	return libs, nil
}

// parseLddLine extracts the resolved file path from one line of `ldd`
// output, e.g. "	libc.so.6 => /lib/x86_64-linux-gnu/libc.so.6 (0x...)"
// or "	/lib64/ld-linux-x86-64.so.2 (0x...)". Returns "" for virtual
// entries like "linux-vdso.so.1 (0x...)" that have no "=>" and don't
// start with "/".
func parseLddLine(line string) string {
	fields := strings.Fields(line)
	for i, f := range fields {
		if f == "=>" && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	if len(fields) > 0 && strings.HasPrefix(fields[0], "/") {
		return fields[0]
	}
	return ""
}

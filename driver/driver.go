// Package driver defines the VMM backend contract the agent's core loop
// dispatches Space create/delete commands through (draft/micro-machine.md
// §4). The agent is deliberately dumb about *how* a Space is actually
// booted — Driver is the seam that lets Firecracker, Cloud Hypervisor, and
// a Mock (for tests / environments without KVM) all satisfy the same
// contract.
package driver

import "context"

// Resources mirrors gox-apps/libs/scheduler/nodes.Resources — duplicated
// here rather than imported so the agent binary has zero dependency on the
// scheduler's Go module (it only ever talks to it over HTTP, per draft/
// micro-machine.md §6's SchedulerClient interface). JSON tags must match
// gox-apps/libs/fleet/nodes.Resources exactly (camelCase, not snake_case)
// — this crosses the wire in both directions (Enroll/Heartbeat capacity
// reports, and the resources embedded in a create_space Command's
// VMSpec), and a tag mismatch silently zeros out MemMB/DiskGB/GPUModel on
// whichever side decodes it rather than erroring (SANDBOX.md §21).
type Resources struct {
	VCPUs    int    `json:"vcpus"`
	MemMB    int    `json:"memMb"`
	DiskGB   int    `json:"diskGb"`
	GPUs     int    `json:"gpus"`
	GPUModel string `json:"gpuModel"`
}

// VMSpec is what a create_space Command's payload decodes into — enough
// to boot a microVM and make it reachable.
type VMSpec struct {
	SpaceID      string    `json:"space_id"`
	Image        string    `json:"image"`
	Type         string    `json:"type"` // vm | container — empty is treated as "vm" by VM-only drivers, for backward compatibility with callers that predate this field
	Resources    Resources `json:"resources"`
	SSHPublicKey string    `json:"ssh_public_key"`
	EnvID        string    `json:"env_id"` // keys the per-tenant tap/subnet, draft/micro-machine.md §5
}

// Instance is what Driver.Create reports back — becomes the command
// result the agent posts to /v1/agents/{id}/results.
type Instance struct {
	ID      string `json:"id"` // same as VMSpec.SpaceID
	IP      string `json:"ip"`
	SSHPort int    `json:"ssh_port"`
}

// InstanceState is Driver.List's ground truth — what's actually running,
// independent of what the agent's in-memory view believes (used for
// startup reconciliation, draft/micro-machine.md §7).
type InstanceState struct {
	ID      string `json:"id"`
	Status  string `json:"status"` // running | stopped | unknown
	IP      string `json:"ip"`
	SSHPort int    `json:"ssh_port"`
}

// InstanceStats backs the agent_cgroup_usage_ratio metric and heartbeat's
// actual-usage report — cross-checked against the scheduler's own
// accounting, never trusted from the guest (draft/micro-machine.md §4, §9).
type InstanceStats struct {
	CPUPercent float64 `json:"cpu_percent"`
	MemUsedMB  int     `json:"mem_used_mb"`
	DiskUsedGB int     `json:"disk_used_gb"`
}

// Driver is the VMM backend contract — draft/micro-machine.md §4, verbatim
// interface shape.
type Driver interface {
	Name() string
	Supports(spec VMSpec) bool
	Create(ctx context.Context, spec VMSpec) (*Instance, error)
	Delete(ctx context.Context, id string) error
	List(ctx context.Context) ([]InstanceState, error)
	Stats(ctx context.Context, id string) (InstanceStats, error)
}

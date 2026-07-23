package driver

import (
	"context"
	"errors"
)

// Container is the backend for container-type Spaces (draft/
// micro-machine.md §4's VMM-backend seam, extended beyond microVMs): a
// lighter-weight OCI runtime (runc/containerd) instead of a full VM,
// trading Firecracker/Cloud Hypervisor's stronger isolation for faster
// boot and lower per-Space overhead.
//
// Not implemented in this pass — same reasoning as Firecracker/Cloud
// Hypervisor: real namespace/cgroup/OCI-bundle orchestration needs a
// Linux host, not available to develop/verify against in this
// environment. The interface shape is real; the bodies are stubs.
type Container struct{}

func NewContainer() *Container { return &Container{} }

func (c *Container) Name() string { return "container" }

// Supports accepts only Spaces explicitly requesting the "container"
// type. GPU passthrough into a plain container needs a GPU-aware runtime
// (e.g. nvidia-container-runtime) that this driver doesn't implement, so
// GPU requests are rejected too.
func (c *Container) Supports(spec VMSpec) bool {
	return spec.Type == "container" && spec.Resources.GPUs == 0
}

func (c *Container) Create(ctx context.Context, spec VMSpec) (*Instance, error) {
	return nil, errors.New("container driver: not implemented (requires a Linux host with an OCI runtime)")
}

func (c *Container) Delete(ctx context.Context, id string) error {
	return errors.New("container driver: not implemented")
}

func (c *Container) List(ctx context.Context) ([]InstanceState, error) {
	return nil, errors.New("container driver: not implemented")
}

func (c *Container) Stats(ctx context.Context, id string) (InstanceStats, error) {
	return InstanceStats{}, errors.New("container driver: not implemented")
}

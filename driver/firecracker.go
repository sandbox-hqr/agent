package driver

import (
	"context"
	"errors"
)

// Firecracker is the default backend for CPU workloads (draft/
// micro-machine.md §4): ~125ms boot, tiny memory overhead, strong
// isolation via jailer + seccomp + cgroups.
//
// Not implemented in this pass — real Firecracker orchestration (spawning
// the jailer, configuring the VMM over its REST-over-unix-socket API,
// allocating a tap device on a per-env bridge, building a copy-on-write
// rootfs overlay, injecting the SSH key + cloud-init metadata, applying
// cgroup hard caps) requires a Linux host with KVM, which isn't available
// to develop/verify against in this environment. The interface shape and
// the critical GPU constraint below are real; the bodies are stubs.
type Firecracker struct {
	// BinaryPath, JailerPath, socket/image-cache directories, etc. would
	// live here — see draft/micro-machine.md §10's driver.* config keys.
}

func NewFirecracker() *Firecracker { return &Firecracker{} }

func (f *Firecracker) Name() string { return "firecracker" }

// Supports returns false whenever GPUs are requested — Firecracker has no
// PCI passthrough, a hard hardware constraint, not a policy choice
// (draft/micro-machine.md §4).
func (f *Firecracker) Supports(spec VMSpec) bool {
	return spec.Resources.GPUs == 0
}

func (f *Firecracker) Create(ctx context.Context, spec VMSpec) (*Instance, error) {
	return nil, errors.New("firecracker driver: not implemented (requires a Linux+KVM host to build and verify against)")
}

func (f *Firecracker) Delete(ctx context.Context, id string) error {
	return errors.New("firecracker driver: not implemented")
}

func (f *Firecracker) List(ctx context.Context) ([]InstanceState, error) {
	return nil, errors.New("firecracker driver: not implemented")
}

func (f *Firecracker) Stats(ctx context.Context, id string) (InstanceStats, error) {
	return InstanceStats{}, errors.New("firecracker driver: not implemented")
}

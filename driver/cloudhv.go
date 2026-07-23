package driver

import (
	"context"
	"errors"
)

// CloudHypervisor is the GPU-workload backend (draft/micro-machine.md §4),
// using VFIO passthrough to hand a physical GPU to a single guest. The
// scheduler bundle only routes GPU requests to Nodes that advertise GPUs
// and run this driver.
//
// Not implemented in this pass — same reasoning as Firecracker: real VFIO
// passthrough orchestration needs a Linux host with an IOMMU-capable GPU,
// not available to verify against here. Phase 2 scope (SANDBOX.md §9) is
// the Mock-driver path; this is Phase 6 hardening work.
type CloudHypervisor struct{}

func NewCloudHypervisor() *CloudHypervisor { return &CloudHypervisor{} }

func (c *CloudHypervisor) Name() string { return "cloud-hypervisor" }

func (c *CloudHypervisor) Supports(spec VMSpec) bool {
	return true // fractional GPU (MIG) selection would be decided here — see §4's GPU sharing note
}

func (c *CloudHypervisor) Create(ctx context.Context, spec VMSpec) (*Instance, error) {
	return nil, errors.New("cloud-hypervisor driver: not implemented (requires a Linux host with an IOMMU-capable GPU)")
}

func (c *CloudHypervisor) Delete(ctx context.Context, id string) error {
	return errors.New("cloud-hypervisor driver: not implemented")
}

func (c *CloudHypervisor) List(ctx context.Context) ([]InstanceState, error) {
	return nil, errors.New("cloud-hypervisor driver: not implemented")
}

func (c *CloudHypervisor) Stats(ctx context.Context, id string) (InstanceStats, error) {
	return InstanceStats{}, errors.New("cloud-hypervisor driver: not implemented")
}

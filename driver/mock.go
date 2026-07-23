package driver

import (
	"context"
	"fmt"
	"net"
	"sync"
)

// Mock fakes a VM with a loopback listener so the whole system runs and is
// testable without KVM (draft/micro-machine.md §4) — also used in the
// agent's own tests for the reconciliation and command-execution loops.
type Mock struct {
	mu        sync.Mutex
	instances map[string]*mockInstance
}

type mockInstance struct {
	listener net.Listener
	port     int
}

func NewMock() *Mock {
	return &Mock{instances: make(map[string]*mockInstance)}
}

func (m *Mock) Name() string { return "mock" }

// Supports is unconditionally true — Mock never has real hardware
// constraints, unlike Firecracker (§4's "no GPU passthrough" rule).
func (m *Mock) Supports(spec VMSpec) bool { return true }

func (m *Mock) Create(ctx context.Context, spec VMSpec) (*Instance, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if existing, ok := m.instances[spec.SpaceID]; ok {
		return &Instance{ID: spec.SpaceID, IP: "127.0.0.1", SSHPort: existing.port}, nil
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("mock driver: listen: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed on Delete
			}
			conn.Close() // fake guest: accept and hang up, just proves reachability
		}
	}()

	m.instances[spec.SpaceID] = &mockInstance{listener: ln, port: port}
	return &Instance{ID: spec.SpaceID, IP: "127.0.0.1", SSHPort: port}, nil
}

func (m *Mock) Delete(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	inst, ok := m.instances[id]
	if !ok {
		return nil // idempotent — matches draft/micro-machine.md §7's redelivery expectations
	}
	delete(m.instances, id)
	return inst.listener.Close()
}

func (m *Mock) List(ctx context.Context) ([]InstanceState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]InstanceState, 0, len(m.instances))
	for id, inst := range m.instances {
		out = append(out, InstanceState{ID: id, Status: "running", IP: "127.0.0.1", SSHPort: inst.port})
	}
	return out, nil
}

func (m *Mock) Stats(ctx context.Context, id string) (InstanceStats, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.instances[id]; !ok {
		return InstanceStats{}, fmt.Errorf("mock driver: no such instance %q", id)
	}
	// Fake but plausible — Mock has no real cgroup to read.
	return InstanceStats{CPUPercent: 0.5, MemUsedMB: 64, DiskUsedGB: 1}, nil
}

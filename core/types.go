package core

import (
	"context"
	"time"

	"github.com/sandbox-hq/agent/driver"
)

// AgentCredentials is Enroll's result — the bearer token is persisted
// locally (state.go) and never logged in full (draft/micro-machine.md §8).
type AgentCredentials struct {
	NodeID      string `json:"node_id"`
	BearerToken string `json:"bearer_token"`
}

// HeartbeatReport carries the agent's actual allocation and running-Space
// list — the Driver.List ground truth reconcile diffs against desired
// state (draft/scheduler.md §7).
type HeartbeatReport struct {
	Allocated driver.Resources `json:"allocated"`
	SpaceIDs  []string         `json:"space_ids"`
}

type HeartbeatAck struct {
	Acknowledged bool `json:"acknowledged"`
}

// Command mirrors gox-apps/libs/scheduler/nodes.Command's wire shape.
type Command struct {
	ID      string          `json:"id"`
	Type    string          `json:"type"` // create_space | delete_space | create_export | delete_export
	Payload driver.VMSpec   `json:"payload"`
}

type CommandResult struct {
	CommandID string `json:"command_id"`
	Success   bool   `json:"success"`
	Result    any    `json:"result"`
}

// SchedulerClient is the agent's Go-level view of the scheduler bundle —
// concrete implementations are HTTP clients; the interface exists so the
// core loop and its tests don't care about wire format
// (draft/micro-machine.md §6).
type SchedulerClient interface {
	Enroll(ctx context.Context, joinToken string, capacity driver.Resources, hostname string) (AgentCredentials, error)
	Heartbeat(ctx context.Context, nodeID string, report HeartbeatReport) (HeartbeatAck, error)
	// NextCommand returns (nil, nil) on an ordinary long-poll timeout —
	// not an error (draft/micro-machine.md §7).
	NextCommand(ctx context.Context, nodeID string, longPollTimeout time.Duration) (*Command, error)
	ReportResult(ctx context.Context, nodeID string, result CommandResult) error
}

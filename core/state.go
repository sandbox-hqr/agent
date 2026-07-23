package core

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// State is the agent's only local state — created 0600, owned by the
// service user (draft/micro-machine.md §7 Appendix). The scheduler's
// Postgres store is the source of truth; this is just enough to survive
// the agent's own restart.
type State struct {
	NodeID           string          `json:"agent_id"`
	BearerToken      string          `json:"bearer_token"`
	EnrolledAt       time.Time       `json:"enrolled_at"`
	ExecutedCommands map[string]bool `json:"executed_commands,omitempty"` // idempotency log, §7

	mu   sync.Mutex `json:"-"`
	path string
}

func LoadOrNewState(path string) (*State, error) {
	s := &State{path: path, ExecutedCommands: make(map[string]bool)}

	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, s); err != nil {
		return nil, err
	}
	if s.ExecutedCommands == nil {
		s.ExecutedCommands = make(map[string]bool)
	}
	return s, nil
}

func (s *State) Enrolled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.NodeID != "" && s.BearerToken != ""
}

func (s *State) SetCredentials(nodeID, bearerToken string) error {
	s.mu.Lock()
	s.NodeID = nodeID
	s.BearerToken = bearerToken
	s.EnrolledAt = time.Now().UTC()
	s.mu.Unlock()
	return s.save()
}

// AlreadyExecuted checks the idempotency log before a (re)delivered
// command is acted on — draft/micro-machine.md §7's "command idempotency":
// a redelivered command checks existing state first instead of blindly
// re-executing.
func (s *State) AlreadyExecuted(commandID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ExecutedCommands[commandID]
}

func (s *State) MarkExecuted(commandID string) error {
	s.mu.Lock()
	s.ExecutedCommands[commandID] = true
	s.mu.Unlock()
	return s.save()
}

func (s *State) save() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, raw, 0600)
}

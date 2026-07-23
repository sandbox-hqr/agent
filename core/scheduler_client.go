package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/sandbox-hq/agent/driver"
)

// envelope mirrors the {success, data, message} shape every scheduler
// response uses (gox-apps/libs/scheduler, io/output.JSONOutput.Data()).
type envelope struct {
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data"`
	Message string          `json:"message"`
}

// HTTPSchedulerClient is the real SchedulerClient implementation — talks
// to exactly the wire contract gox-apps/libs/scheduler/nodes exposes:
// POST /v1/enroll, /v1/agents/{id}/heartbeat, GET .../commands,
// POST .../results (draft/micro-machine.md §6; draft/scheduler.md §3).
type HTTPSchedulerClient struct {
	BaseURL     string
	BearerToken string // set after Enroll; empty for the Enroll call itself
	HTTPClient  *http.Client
}

func NewHTTPSchedulerClient(baseURL string) *HTTPSchedulerClient {
	return &HTTPSchedulerClient{
		BaseURL:    baseURL,
		HTTPClient: &http.Client{Timeout: 45 * time.Second},
	}
}

func (c *HTTPSchedulerClient) Enroll(ctx context.Context, joinToken string, capacity driver.Resources, hostname string) (AgentCredentials, error) {
	body, _ := json.Marshal(map[string]any{
		"join_token": joinToken,
		"capacity":   capacity,
		"hostname":   hostname,
	})

	var creds AgentCredentials
	if err := c.do(ctx, http.MethodPost, "/v1/enroll", body, false, &creds); err != nil {
		return AgentCredentials{}, err
	}
	return creds, nil
}

func (c *HTTPSchedulerClient) Heartbeat(ctx context.Context, nodeID string, report HeartbeatReport) (HeartbeatAck, error) {
	body, _ := json.Marshal(report)

	var ack HeartbeatAck
	if err := c.do(ctx, http.MethodPost, "/v1/agents/"+nodeID+"/heartbeat", body, true, &ack); err != nil {
		return HeartbeatAck{}, err
	}
	return ack, nil
}

// NextCommand's HTTP timeout must exceed longPollTimeout, or a slow/absent
// command looks like a transport error rather than an ordinary empty
// long-poll (draft/scheduler.md §3's timeout-layering callout — the same
// concern applies symmetrically on the client side).
func (c *HTTPSchedulerClient) NextCommand(ctx context.Context, nodeID string, longPollTimeout time.Duration) (*Command, error) {
	reqCtx, cancel := context.WithTimeout(ctx, longPollTimeout+10*time.Second)
	defer cancel()

	path := fmt.Sprintf("/v1/agents/%s/commands?timeout=%d", nodeID, int(longPollTimeout.Seconds()))
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.BearerToken)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNoContent {
		return nil, nil // ordinary timeout — draft/micro-machine.md §7: not a failure
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("next command: unexpected status %d: %s", resp.StatusCode, raw)
	}

	var env envelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, err
	}
	if !env.Success {
		return nil, fmt.Errorf("next command: %s", env.Message)
	}

	var cmd Command
	if err := json.Unmarshal(env.Data, &cmd); err != nil {
		return nil, err
	}
	return &cmd, nil
}

func (c *HTTPSchedulerClient) ReportResult(ctx context.Context, nodeID string, result CommandResult) error {
	body, _ := json.Marshal(result)
	return c.do(ctx, http.MethodPost, "/v1/agents/"+nodeID+"/results", body, true, nil)
}

func (c *HTTPSchedulerClient) do(ctx context.Context, method, path string, body []byte, authed bool, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if authed {
		req.Header.Set("Authorization", "Bearer "+c.BearerToken)
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("unexpected response (status %d): %s", resp.StatusCode, raw)
	}
	if !env.Success {
		return fmt.Errorf("%s %s: %s", method, path, env.Message)
	}
	if out != nil {
		return json.Unmarshal(env.Data, out)
	}
	return nil
}

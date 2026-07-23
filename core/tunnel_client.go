package core

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/hashicorp/yamux"
)

// streamHeader mirrors gox-apps/libs/reverse-tunnel-broker/rtb.StreamHeader
// byte-for-byte (draft/reverse-tunnel-broker.md §4) — duplicated rather
// than imported so the agent module has zero dependency on gox-apps' Go
// module graph; it's an independently-deployed binary that only ever talks
// to the scheduler/RTB over the network, never as a library consumer.
type streamHeader struct {
	ExportID   string `json:"export_id"`
	TargetHost string `json:"target_host"`
	TargetPort int    `json:"target_port"`
	Protocol   string `json:"protocol"`
}

func decodeStreamHeader(r io.Reader) (streamHeader, error) {
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return streamHeader{}, err
	}
	n := binary.BigEndian.Uint32(lenBuf[:])
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return streamHeader{}, err
	}
	var h streamHeader
	err := json.Unmarshal(body, &h)
	return h, err
}

// Stream is one logical tunnel — RTB opens it, the agent Accept()s it,
// reads the header, dials the guest port locally, and splices.
type Stream interface {
	io.ReadWriteCloser
}

// Session is the agent's client-side handle on its one persistent
// multiplexed connection to RTB (draft/reverse-tunnel-broker.md §4).
type Session interface {
	// Accept blocks until RTB opens a new stream, or ctx is done.
	Accept(ctx context.Context) (Stream, error)
	Close() error
}

// TunnelClient establishes that session — draft/micro-machine.md §6.
type TunnelClient interface {
	EstablishSession(ctx context.Context, nodeID, bearerToken string) (Session, error)
}

// HTTPTunnelClient implements the handshake RTB's sessionManager.
// SessionHandler expects (gox-apps/libs/reverse-tunnel-broker/rtb/
// session.go): dial raw, write the HTTP request by hand, read just the
// response status line, then treat the same net.Conn as a yamux client —
// net/http's Client can't do this hijack-after-response dance itself.
type HTTPTunnelClient struct {
	// SessionURL is the full tunnel-session endpoint, e.g.
	// "https://cloud.example.com/v1/tunnel/session" (POOLMESH_RTB_URL,
	// draft/micro-machine.md §10 — defaults to the control-plane base URL
	// since the ingress/proxy path-routes this one path to RTB).
	SessionURL string
	TLSConfig  *tls.Config
	DialTimeout time.Duration
}

func NewHTTPTunnelClient(sessionURL string) *HTTPTunnelClient {
	return &HTTPTunnelClient{SessionURL: sessionURL, DialTimeout: 10 * time.Second}
}

func (c *HTTPTunnelClient) EstablishSession(ctx context.Context, nodeID, bearerToken string) (Session, error) {
	u, err := url.Parse(c.SessionURL)
	if err != nil {
		return nil, fmt.Errorf("tunnel client: invalid session url: %w", err)
	}

	conn, err := c.dial(ctx, u)
	if err != nil {
		return nil, fmt.Errorf("tunnel client: dial: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, c.SessionURL, nil)
	if err != nil {
		conn.Close()
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+bearerToken)
	req.Header.Set("X-Node-Id", nodeID)

	if err := req.Write(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("tunnel client: write request: %w", err)
	}

	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("tunnel client: read response: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		conn.Close()
		return nil, fmt.Errorf("tunnel client: session establish failed: HTTP %d", resp.StatusCode)
	}

	// DefaultConfig() already sets a usable LogOutput — yamux.VerifyConfig
	// requires exactly one of Logger/LogOutput to be non-nil, so leave it
	// as-is rather than nil-ing it out.
	yamuxCfg := yamux.DefaultConfig()
	session, err := yamux.Client(conn, yamuxCfg)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("tunnel client: yamux handshake: %w", err)
	}

	return &yamuxSession{session: session}, nil
}

func (c *HTTPTunnelClient) dial(ctx context.Context, u *url.URL) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: c.DialTimeout}
	host := u.Host
	if !strings.Contains(host, ":") {
		if u.Scheme == "https" {
			host += ":443"
		} else {
			host += ":80"
		}
	}

	if u.Scheme == "https" {
		return tls.DialWithDialer(dialer, "tcp", host, c.TLSConfig)
	}
	return dialer.DialContext(ctx, "tcp", host)
}

type yamuxSession struct {
	session *yamux.Session
}

func (s *yamuxSession) Accept(ctx context.Context) (Stream, error) {
	stream, err := s.session.AcceptStreamWithContext(ctx)
	if err != nil {
		return nil, err
	}
	return stream, nil
}

func (s *yamuxSession) Close() error {
	return s.session.Close()
}

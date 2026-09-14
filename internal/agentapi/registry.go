package agentapi

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// connRegistry tracks every authenticated WebSocket so revocation and
// shutdown can close them. Each connection has its own entry: closing one
// tab never unregisters another tab of the same device.
type connRegistry struct {
	mu    sync.Mutex
	conns map[*wsConn]struct{}
}

// wsConn is one registered WebSocket.
type wsConn struct {
	deviceID string
	kind     string // "tty"
	ws       *websocket.Conn
	closed   atomic.Bool
	once     sync.Once
}

func newConnRegistry() *connRegistry {
	return &connRegistry{conns: map[*wsConn]struct{}{}}
}

// Register records a live WebSocket for deviceID. The caller must call
// Unregister when the connection ends.
func (r *connRegistry) Register(deviceID, kind string, ws *websocket.Conn) *wsConn {
	c := &wsConn{deviceID: deviceID, kind: kind, ws: ws}
	r.mu.Lock()
	r.conns[c] = struct{}{}
	r.mu.Unlock()
	return c
}

// Unregister forgets c. Idempotent.
func (r *connRegistry) Unregister(c *wsConn) {
	r.mu.Lock()
	delete(r.conns, c)
	r.mu.Unlock()
}

// CloseDevice closes every connection of deviceID and returns how many.
func (r *connRegistry) CloseDevice(deviceID string) int {
	return r.closeWhere(func(c *wsConn) bool { return c.deviceID == deviceID }, "device revoked")
}

// CloseAll closes every registered connection and returns how many.
func (r *connRegistry) CloseAll() int {
	return r.closeWhere(func(*wsConn) bool { return true }, "server shutting down")
}

// Len reports how many connections are registered.
func (r *connRegistry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.conns)
}

func (r *connRegistry) closeWhere(match func(*wsConn) bool, reason string) int {
	r.mu.Lock()
	var hit []*wsConn
	for c := range r.conns {
		if match(c) {
			hit = append(hit, c)
			delete(r.conns, c)
		}
	}
	r.mu.Unlock()
	for _, c := range hit {
		c.Close(reason)
	}
	return len(hit)
}

// Closed reports whether the connection has been closed by the server. The
// handlers check it before acting on a frame, so input that was already read
// when a revocation landed never reaches a PTY.
func (c *wsConn) Closed() bool { return c.closed.Load() }

// Close marks the connection closed, sends a close frame with a short
// deadline (a wedged client cannot hang the caller) and closes the network
// connection, which ends the handler's read loop.
func (c *wsConn) Close(reason string) {
	c.once.Do(func() {
		c.closed.Store(true)
		_ = c.ws.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.ClosePolicyViolation, reason),
			time.Now().Add(time.Second))
		_ = c.ws.Close()
	})
}

// watchDevice re-verifies the connection's device token every s.wsReverifyEvery
// until ctx ends, closing the connection as soon as the token stops being
// valid. It covers revocations that do not go through RevokeDevice (another
// process editing the store, token expiry).
func (s *Server) watchDevice(ctx context.Context, c *wsConn, token string) {
	t := time.NewTicker(s.wsReverifyEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d, err := s.st.VerifyClientDevice(ctx, token)
			if ctx.Err() != nil {
				return
			}
			if err != nil || d == nil || d.ID != c.deviceID {
				s.log.Info("agentd: closing websocket of a device that is no longer valid",
					"deviceId", c.deviceID, "kind", c.kind)
				s.wsRegistry.Unregister(c)
				c.Close("device no longer authorized")
				return
			}
		}
	}
}

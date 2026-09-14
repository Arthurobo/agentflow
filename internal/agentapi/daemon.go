package agentapi

import (
	"context"
	"time"

	"github.com/arthurobo/agentflow/internal/store"
)

// PairingTTL is how long a minted pairing token stays redeemable.
const PairingTTL = 15 * time.Minute

// MintPairingToken stores a one-shot pairing token and returns it with its
// expiry in unix milliseconds. Only the token's hash is stored. There is no
// HTTP route for this: the CLI reaches it through the admin socket, so minting
// requires the daemon user's filesystem permissions.
func (s *Server) MintPairingToken(ctx context.Context) (token string, expiresAt int64, err error) {
	token = newSecret(24)
	id := "pair-" + newSecret(6)
	expiresAt = time.Now().Add(PairingTTL).UnixMilli()
	dev := &store.Device{
		ID: id, Name: "pairing-" + id[:6], MachineID: s.machineID,
		Kind: "pairing", Status: "pending", TokenHash: store.HashToken(token),
	}
	// The expiry must reach the row: a zero expires_at means "never expires".
	if err := s.upsertDeviceWithRetry(ctx, dev, expiresAt); err != nil {
		return "", 0, err
	}
	s.log.Info("pair: token minted", "deviceId", id, "expiresAt", expiresAt)
	return token, expiresAt, nil
}

// RevokeDevice revokes a device in the store and closes its live connections.
// It returns how many connections were closed.
func (s *Server) RevokeDevice(ctx context.Context, id string) (closed int, err error) {
	if err := s.st.RevokeDevice(ctx, id); err != nil {
		return 0, err
	}
	closed = s.wsRegistry.CloseDevice(id)
	if closed > 0 {
		s.log.Info("agentd: closed live connections of revoked device", "deviceId", id, "connections", closed)
	}
	return closed, nil
}

// CloseAllConnections closes every live WebSocket (terminal sockets). The
// daemon calls it on shutdown so no socket or listener outlives the server.
func (s *Server) CloseAllConnections() int {
	return s.wsRegistry.CloseAll()
}

// SetPublicOrigin tells the server how to learn the current public origin
// (for example https://agentflow-3fa9c1.tail1234.ts.net) so WebSocket Origin
// checks can allow it. fn returns "" while remote access is not running.
func (s *Server) SetPublicOrigin(fn func() string) {
	s.mu.Lock()
	s.publicOrigin = fn
	s.mu.Unlock()
}

// PublicOrigin returns the current public origin, or "" when there is none.
func (s *Server) PublicOrigin() string {
	s.mu.Lock()
	fn := s.publicOrigin
	s.mu.Unlock()
	if fn == nil {
		return ""
	}
	return fn()
}

// SetListenAddr records the local listener's configured address (host:port).
// WebSocket Origin checks accept pages served from that address and from the
// loopback names on its port. httpserve.Root calls it for the local root.
func (s *Server) SetListenAddr(addr string) {
	s.mu.Lock()
	s.listenAddr = addr
	s.mu.Unlock()
}

func (s *Server) listenAddress() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listenAddr
}

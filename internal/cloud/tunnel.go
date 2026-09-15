package cloud

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// A Cloudflare tunnel for this machine comes from the account service: the
// daemon sends its machine id, its hostname and a secret it generated, and
// gets back a hostname under the service's domain and the token cloudflared
// runs the tunnel with. No sign-in is needed. The secret is written to disk
// before the first request, so an answer lost on the way back is recovered by
// asking again; the service refuses the machine id with any other secret.
// The last good hostname and token are kept next to it, so the tunnel still
// comes up while the service can't be reached.

// tunnelFile holds the secret and the last grant, owner-only.
const tunnelFile = "cloudflare-tunnel.json"

// TunnelFileName is the tunnel state file inside the data directory, for
// uninstall.
const TunnelFileName = tunnelFile

// ErrTunnelSecretMismatch means the service has a tunnel for this machine id
// under a different secret: the secret file was lost or replaced while the
// machine id was kept.
var ErrTunnelSecretMismatch = errors.New("cloud: this machine id already has a tunnel under another secret")

// TunnelGrant is a machine's tunnel as the service hands it out.
type TunnelGrant struct {
	Hostname string
	Token    string
}

// ProvisionClient is the provisioning call of the account service.
type ProvisionClient interface {
	ProvisionTunnel(ctx context.Context, machineID, hostname, secret string) (TunnelGrant, error)
}

// NewProvisionClient returns a ProvisionClient for baseURL (see NewClient).
func NewProvisionClient(baseURL string) ProvisionClient {
	return NewClient(baseURL).(*httpClient)
}

var tunnelHostRE = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)

func (c *httpClient) ProvisionTunnel(ctx context.Context, machineID, hostname, secret string) (TunnelGrant, error) {
	var resp struct {
		Hostname    string `json:"hostname"`
		TunnelToken string `json:"tunnelToken"`
		Transport   string `json:"transport"`
	}
	body := map[string]string{"machineId": machineID, "hostname": hostname, "provisionSecret": secret}
	if err := c.do(ctx, "POST", "/v1/provision", "", body, &resp); err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Code == "machine_provisioned" {
			return TunnelGrant{}, fmt.Errorf("%w (%w)", ErrTunnelSecretMismatch, err)
		}
		return TunnelGrant{}, err
	}
	host := strings.ToLower(resp.Hostname)
	if !tunnelHostRE.MatchString(host) || resp.TunnelToken == "" || strings.ContainsAny(resp.TunnelToken, " \t\r\n") {
		return TunnelGrant{}, errors.New("cloud: account service returned an unusable tunnel")
	}
	return TunnelGrant{Hostname: host, Token: resp.TunnelToken}, nil
}

// tunnelState is the tunnel file.
type tunnelState struct {
	Secret   string `json:"provisionSecret"`
	Hostname string `json:"hostname,omitempty"`
	Token    string `json:"tunnelToken,omitempty"`
}

func loadTunnelState(dataDir string) (tunnelState, error) {
	var st tunnelState
	b, err := os.ReadFile(filepath.Join(dataDir, tunnelFile))
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, fmt.Errorf("read %s: %w", tunnelFile, err)
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return tunnelState{}, fmt.Errorf("read %s: %w", filepath.Join(dataDir, tunnelFile), err)
	}
	return st, nil
}

func saveTunnelState(dataDir string, st tunnelState) error {
	b, err := json.MarshalIndent(st, "", "  ") //nolint:gosec // the secret belongs in this owner-only file
	if err != nil {
		return err
	}
	return writeFileAtomic(dataDir, tunnelFile, append(b, '\n'))
}

// TunnelSource gets this machine's tunnel from the account service and keeps
// the last good one.
type TunnelSource struct {
	DataDir string
	// MachineName is the name the service records (the OS hostname).
	MachineName string
	Client      ProvisionClient
}

// Cached returns the last tunnel the service granted, if any.
func (s *TunnelSource) Cached() (TunnelGrant, bool) {
	st, err := loadTunnelState(s.DataDir)
	if err != nil || st.Hostname == "" || st.Token == "" {
		return TunnelGrant{}, false
	}
	return TunnelGrant{Hostname: st.Hostname, Token: st.Token}, true
}

// Fetch asks the service for this machine's tunnel, creating the secret
// first if there is none, and stores the answer.
func (s *TunnelSource) Fetch(ctx context.Context) (TunnelGrant, error) {
	id, err := MachineID(s.DataDir)
	if err != nil {
		return TunnelGrant{}, err
	}
	st, err := loadTunnelState(s.DataDir)
	if err != nil {
		return TunnelGrant{}, err
	}
	if st.Secret == "" {
		if st.Secret, err = newTunnelSecret(); err != nil {
			return TunnelGrant{}, err
		}
		if err := saveTunnelState(s.DataDir, st); err != nil {
			return TunnelGrant{}, fmt.Errorf("save the tunnel secret: %w", err)
		}
	}
	g, err := s.Client.ProvisionTunnel(ctx, id, s.MachineName, st.Secret)
	if err != nil {
		return TunnelGrant{}, err
	}
	if g.Hostname != st.Hostname || g.Token != st.Token {
		st.Hostname, st.Token = g.Hostname, g.Token
		if err := saveTunnelState(s.DataDir, st); err != nil {
			return TunnelGrant{}, fmt.Errorf("save the tunnel: %w", err)
		}
	}
	return g, nil
}

// newTunnelSecret returns 32 random bytes as unpadded base64url.
func newTunnelSecret() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

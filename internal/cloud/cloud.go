// Package cloud is the daemon's and CLI's client for the agentflow account
// service: optional email sign-in, and
// reporting each machine's current public URL so the user can be emailed the
// link and see it on the dashboard.
//
// The service never receives device tokens, pairing tokens or anything that
// grants access to a machine. It knows an email, a machine id and name, the
// transport, and the current URL.
package cloud

import (
	"errors"
	"strings"
	"time"
)

// ErrNotImplemented was returned before the account client existed. Nothing
// returns it any more; it stays so code written against the first contract
// keeps compiling.
var ErrNotImplemented = errors.New("cloud: not implemented yet")

// ErrSkipped is returned by PromptEmail when the user skipped an optional
// email.
var ErrSkipped = errors.New("cloud: email skipped")

// Account is the signed-in account stored locally (owner-only file under the
// data directory).
type Account struct {
	Email      string    `json:"email"`
	Token      string    `json:"token"`
	MachineID  string    `json:"machineId"`
	VerifiedAt time.Time `json:"verifiedAt"`
}

// Machine is what the daemon reports about itself.
type Machine struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Transport string `json:"transport"` // "tailscale"
	URL       string `json:"url"`
	Version   string `json:"agentflowVersion"`
}

// URLStatus is one observation of the machine's public URL. URL is empty
// while the transport is not running.
type URLStatus struct {
	Transport string
	URL       string
}

// DefaultBaseURL is the production account service.
const DefaultBaseURL = "https://cloud.useagentflow.xyz"

// Version is the agentflow version sent in the User-Agent header. The
// agentflow binary sets it at startup.
var Version = "dev"

// BaseURLFromEnv returns AF_CLOUD_URL from getenv, or DefaultBaseURL.
func BaseURLFromEnv(getenv func(string) string) string {
	if v := strings.TrimSpace(getenv("AF_CLOUD_URL")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return DefaultBaseURL
}

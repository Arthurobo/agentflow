package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
)

// defaultAddr is where the local listener binds unless AF_ADDR or --addr says
// otherwise.
const defaultAddr = "127.0.0.1:4344"

const usage = `agentflow runs coding-agent terminals (Claude Code, OpenCode) in the
background and lets your phone drive them.

USAGE
  agentflow [serve] [--addr ADDR] [--db PATH]
                           run the daemon in the foreground (the default)
  agentflow start [--email ADDR | --no-email]
                           install and start the background service, set up
                           Tailscale and pair a phone
  agentflow stop           stop the service and keep it from starting at login
  agentflow restart        restart the service
  agentflow status         service, health, engines and remote access
  agentflow doctor         check engines, the service and remote access
  agentflow update         install the latest release and restart
  agentflow uninstall [--purge] [--yes]
                           remove the service and binary; --purge also deletes
                           agentflow's data and settings
  agentflow version        build information

  agentflow pair           one-time link and QR code to pair a phone
  agentflow devices        list paired devices and access requests
  agentflow approve CODE   let a device that tapped Request access in (CODE is
                           its four-digit code or its request id)
  agentflow deny CODE      turn an access request down
  agentflow revoke ID      revoke a device and close its live sessions
  agentflow remote status|logout|on|off
                           show, log out of, enable or disable remote access
  agentflow account login|logout|status
                           sign in with your email so this machine's link is
                           emailed to you and listed on the dashboard

  agentflow make-resumable ID        make a session show up in claude -r
  agentflow undo-make-resumable ID   restore it from the backup

CONFIG
  ~/.config/agentflow/agentflow.env   settings (AF_REMOTE, AF_ADDR, ...)
  ~/.local/share/agentflow/           database, uploads, Tailscale state

REMOTE ACCESS
  AF_REMOTE=tailscale (the default) serves an https://<machine>.<tailnet>.ts.net
  URL through the Tailscale on this computer, or agentflow's own node with
  AF_TAILSCALE=embedded; the phone needs no app (Tailscale Funnel), and
  AF_REMOTE_MODE=tailnet keeps it private to your tailnet. Only paired devices
  get past the pairing page. AF_REMOTE=off (agentflow remote off) disables
  remote access.
`

// errUsage marks a command line that doesn't parse; the caller prints usage
// and exits 2.
var errUsage = errors.New("usage")

// invocation is a parsed command line.
type invocation struct {
	Name string // subcommand; "help" prints usage

	// serve
	Addr string
	DB   string

	// uninstall
	Purge bool
	Yes   bool

	// start
	Email   string
	NoEmail bool

	// revoke, approve, deny, make-resumable, undo-make-resumable
	Target string

	// remote
	RemoteVerb string

	// account
	AccountVerb string
}

// parseCommand turns argv (without the program name) into an invocation. It
// has no side effects. env supplies defaults for serve's flags.
func parseCommand(argv []string, env func(string) string) (invocation, error) {
	if len(argv) == 0 {
		return invocation{Name: "serve", Addr: orDefault(env("AF_ADDR"), defaultAddr), DB: env("AF_DB")}, nil
	}
	name, rest := argv[0], argv[1:]
	switch name {
	case "help", "-h", "-help", "--help":
		return invocation{Name: "help"}, nil
	}
	// Flags before the subcommand ("agentflow --addr x") are only meaningful
	// for serve, which is also what an argv starting with a flag means.
	if strings.HasPrefix(name, "-") {
		name, rest = "serve", argv
	}

	inv := invocation{Name: name}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var positional []string

	switch name {
	case "serve":
		fs.StringVar(&inv.Addr, "addr", orDefault(env("AF_ADDR"), defaultAddr), "local listen address")
		fs.StringVar(&inv.DB, "db", env("AF_DB"), "SQLite database path")
	case "uninstall":
		fs.BoolVar(&inv.Purge, "purge", false, "also delete data and settings")
		fs.BoolVar(&inv.Yes, "yes", false, "don't ask for confirmation")
	case "start":
		fs.StringVar(&inv.Email, "email", "", "email for this machine's link")
		fs.BoolVar(&inv.NoEmail, "no-email", false, "don't sign in")
	case "stop", "restart", "status", "doctor", "version", "update", "pair", "devices":
	case "revoke", "approve", "deny", "make-resumable", "undo-make-resumable", "remote", "account":
	default:
		return invocation{}, fmt.Errorf("%w: unknown command %q", errUsage, name)
	}

	if err := parseInterleaved(fs, rest, &positional); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return invocation{Name: "help"}, nil
		}
		return invocation{}, fmt.Errorf("%w: %s: %v", errUsage, name, err)
	}

	switch name {
	case "start":
		if inv.Email != "" && inv.NoEmail {
			return invocation{}, fmt.Errorf("%w: agentflow start takes --email or --no-email, not both", errUsage)
		}
		if len(positional) > 0 {
			return invocation{}, fmt.Errorf("%w: agentflow start takes no arguments (got %q)", errUsage, strings.Join(positional, " "))
		}
	case "revoke", "approve", "deny", "make-resumable", "undo-make-resumable":
		if len(positional) != 1 || positional[0] == "" {
			return invocation{}, fmt.Errorf("%w: agentflow %s needs exactly one id", errUsage, name)
		}
		inv.Target = positional[0]
	case "remote":
		if len(positional) != 1 {
			return invocation{}, fmt.Errorf("%w: agentflow remote needs one of status, logout, on, off", errUsage)
		}
		switch positional[0] {
		case "status", "logout", "on", "off":
			inv.RemoteVerb = positional[0]
		default:
			return invocation{}, fmt.Errorf("%w: agentflow remote %q: expected status, logout, on or off", errUsage, positional[0])
		}
	case "account":
		if len(positional) != 1 {
			return invocation{}, fmt.Errorf("%w: agentflow account needs one of login, logout, status", errUsage)
		}
		switch positional[0] {
		case "login", "logout", "status":
			inv.AccountVerb = positional[0]
		default:
			return invocation{}, fmt.Errorf("%w: agentflow account %q: expected login, logout or status", errUsage, positional[0])
		}
	default:
		if len(positional) > 0 {
			return invocation{}, fmt.Errorf("%w: agentflow %s takes no arguments (got %q)", errUsage, name, strings.Join(positional, " "))
		}
	}
	return inv, nil
}

// parseInterleaved parses flags that may appear before or after positional
// arguments ("revoke dev-1" and "uninstall --yes --purge" both work). A
// literal "--" ends flag parsing.
func parseInterleaved(fs *flag.FlagSet, args []string, positional *[]string) error {
	for {
		if err := fs.Parse(args); err != nil {
			return err
		}
		args = fs.Args()
		if len(args) == 0 {
			return nil
		}
		if args[0] == "--" {
			*positional = append(*positional, args[1:]...)
			return nil
		}
		*positional = append(*positional, args[0])
		args = args[1:]
	}
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

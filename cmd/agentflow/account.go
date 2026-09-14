package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/arthurobo/agentflow/internal/adminsock"
	"github.com/arthurobo/agentflow/internal/cloud"
)

// accountFlow is `agentflow account login|logout|status`: the email sign-in
// that lets the account service email this machine's link and list it on the
// dashboard.
type accountFlow struct {
	in      io.Reader
	out     io.Writer
	client  cloud.Client
	baseURL string
	dataDir string
	// daemonRunning reports whether the agentflow daemon answers on its admin
	// socket.
	daemonRunning func(ctx context.Context) bool
	// reload asks a running daemon to pick up the account file as it is now;
	// nil means it can't, and a restart is suggested instead.
	reload func(ctx context.Context) error
}

func runAccount(cfg config, verb string) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	base := cloud.BaseURLFromEnv(os.Getenv)
	f := &accountFlow{
		in:      os.Stdin,
		out:     os.Stdout,
		client:  cloud.NewClient(base),
		baseURL: base,
		dataDir: cfg.dataDir,
		daemonRunning: func(ctx context.Context) bool {
			ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			// A daemon that holds the socket but doesn't answer is still
			// running.
			err := adminClient(cfg).Do(ctx, "GET", "/health", nil)
			return err == nil || !errors.Is(err, adminsock.ErrNotRunning)
		},
		reload: func(ctx context.Context) error {
			return adminClient(cfg).Do(ctx, "POST", "/account/reload", nil)
		},
	}
	if err := f.run(ctx, verb); err != nil {
		if ctx.Err() != nil {
			fmt.Fprintln(os.Stdout)
			return 130
		}
		fmt.Fprintln(os.Stderr, "agentflow:", err)
		return 1
	}
	return 0
}

func (f *accountFlow) run(ctx context.Context, verb string) error {
	switch verb {
	case "login":
		return f.login(ctx)
	case "logout":
		return f.logout(ctx)
	case "status":
		return f.status(ctx)
	}
	return fmt.Errorf("unknown account command %q", verb)
}

func (f *accountFlow) login(ctx context.Context) error {
	old, err := cloud.LoadAccount(f.dataDir)
	if err != nil {
		// A damaged file is replaced by signing in again.
		old = nil
	}
	if old != nil {
		fmt.Fprintf(f.out, "Signed in as %s. Signing in again replaces that account on this computer.\n", old.Email)
	}
	acct, err := cloud.PromptEmail(ctx, f.in, f.out, f.client, f.dataDir, true)
	if err != nil {
		return err
	}
	if old != nil && old.Token != acct.Token {
		// The previous token isn't stored anywhere any more; revoke it so it
		// doesn't stay valid on the service.
		_ = f.client.Logout(ctx, old.Token)
	}
	fmt.Fprintf(f.out, "Your machines: %s\n", f.baseURL)
	if f.daemonRunning(ctx) {
		if f.reload != nil && f.reload(ctx) == nil {
			fmt.Fprintln(f.out, "agentflow is running and now reports this machine's link.")
		} else {
			fmt.Fprintln(f.out, "agentflow is running; restart it so it starts reporting this machine's link: agentflow restart")
		}
	} else {
		fmt.Fprintln(f.out, "agentflow reports this machine's link the next time it starts.")
	}
	return nil
}

func (f *accountFlow) logout(ctx context.Context) error {
	acct, err := cloud.LoadAccount(f.dataDir)
	if err != nil {
		if derr := cloud.DeleteAccount(f.dataDir); derr != nil {
			return derr
		}
		fmt.Fprintln(f.out, "Removed a damaged sign-in file; this computer is signed out.")
		return nil
	}
	if acct == nil {
		fmt.Fprintln(f.out, "Not signed in.")
		return nil
	}
	revokeErr := f.client.Logout(ctx, acct.Token)
	if err := cloud.DeleteAccount(f.dataDir); err != nil {
		return err
	}
	if revokeErr != nil {
		fmt.Fprintf(f.out, "Signed out of %s on this computer, but the account service couldn't be reached to end the session (%v).\n", acct.Email, revokeErr)
		if f.daemonRunning(ctx) && (f.reload == nil || f.reload(ctx) != nil) {
			fmt.Fprintln(f.out, "Restart agentflow so it stops reporting this machine's link: agentflow restart")
		}
		return nil
	}
	if f.daemonRunning(ctx) && f.reload != nil {
		_ = f.reload(ctx)
	}
	fmt.Fprintf(f.out, "Signed out of %s. This machine no longer reports its link.\n", acct.Email)
	return nil
}

func (f *accountFlow) status(ctx context.Context) error {
	acct, err := cloud.LoadAccount(f.dataDir)
	if err != nil {
		return err
	}
	if acct == nil {
		fmt.Fprintln(f.out, "account     not signed in (run `agentflow account login`)")
		fmt.Fprintln(f.out, "reporting   off")
		return nil
	}
	id := acct.MachineID
	if id == "" {
		if id, err = cloud.MachineID(f.dataDir); err != nil {
			return err
		}
	}
	fmt.Fprintf(f.out, "account     %s\n", acct.Email)
	if !acct.VerifiedAt.IsZero() {
		fmt.Fprintf(f.out, "signed in   %s\n", acct.VerifiedAt.Local().Format("2006-01-02 15:04"))
	}
	fmt.Fprintf(f.out, "machine id  %s\n", id)
	fmt.Fprintf(f.out, "dashboard   %s\n", f.baseURL)
	if f.daemonRunning(ctx) {
		fmt.Fprintln(f.out, "reporting   on while agentflow runs (if you signed in after it started, run `agentflow restart`)")
	} else {
		fmt.Fprintln(f.out, "reporting   off (agentflow isn't running)")
	}
	return nil
}

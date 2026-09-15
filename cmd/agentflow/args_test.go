package main

import (
	"errors"
	"testing"
)

func TestParseCommand(t *testing.T) {
	env := map[string]string{}
	getenv := func(k string) string { return env[k] }

	cases := []struct {
		argv    []string
		want    invocation
		wantErr bool
	}{
		{argv: nil, want: invocation{Name: "serve", Addr: defaultAddr}},
		{argv: []string{"serve"}, want: invocation{Name: "serve", Addr: defaultAddr}},
		{argv: []string{"serve", "--addr", "127.0.0.1:9000", "--db", "/tmp/x.db"}, want: invocation{Name: "serve", Addr: "127.0.0.1:9000", DB: "/tmp/x.db"}},
		{argv: []string{"serve", "-addr=:1"}, want: invocation{Name: "serve", Addr: ":1"}},
		{argv: []string{"--addr", "127.0.0.1:1"}, want: invocation{Name: "serve", Addr: "127.0.0.1:1"}},
		{argv: []string{"serve", "extra"}, wantErr: true},
		{argv: []string{"serve", "--bogus"}, wantErr: true},
		{argv: []string{"start"}, want: invocation{Name: "start"}},
		{argv: []string{"start", "--db", "x"}, wantErr: true},
		{argv: []string{"start", "--no-email"}, want: invocation{Name: "start", NoEmail: true}},
		{argv: []string{"start", "--email", "a@example.com"}, want: invocation{Name: "start", Email: "a@example.com"}},
		{argv: []string{"start", "--transport", "tailscale"}, wantErr: true},
		{argv: []string{"start", "--email", "a@example.com", "--no-email"}, wantErr: true},
		{argv: []string{"start", "now"}, wantErr: true},
		{argv: []string{"stop"}, want: invocation{Name: "stop"}},
		{argv: []string{"restart"}, want: invocation{Name: "restart"}},
		{argv: []string{"status"}, want: invocation{Name: "status"}},
		{argv: []string{"doctor"}, want: invocation{Name: "doctor"}},
		{argv: []string{"version"}, want: invocation{Name: "version"}},
		{argv: []string{"update"}, want: invocation{Name: "update"}},
		{argv: []string{"pair"}, want: invocation{Name: "pair"}},
		{argv: []string{"pair", "--reusable"}, wantErr: true},
		{argv: []string{"devices"}, want: invocation{Name: "devices"}},
		{argv: []string{"uninstall"}, want: invocation{Name: "uninstall"}},
		{argv: []string{"uninstall", "--purge"}, want: invocation{Name: "uninstall", Purge: true}},
		{argv: []string{"uninstall", "--yes", "--purge"}, want: invocation{Name: "uninstall", Purge: true, Yes: true}},
		{argv: []string{"uninstall", "--purge=false", "--yes"}, want: invocation{Name: "uninstall", Yes: true}},
		{argv: []string{"uninstall", "--keep-data"}, want: invocation{Name: "uninstall", KeepData: true}},
		{argv: []string{"uninstall", "--keep-data", "--yes"}, want: invocation{Name: "uninstall", KeepData: true, Yes: true}},
		{argv: []string{"uninstall", "now"}, wantErr: true},
		{argv: []string{"revoke", "dev-1"}, want: invocation{Name: "revoke", Target: "dev-1"}},
		{argv: []string{"revoke"}, wantErr: true},
		{argv: []string{"approve", "4821"}, want: invocation{Name: "approve", Target: "4821"}},
		{argv: []string{"deny", "0f3a"}, want: invocation{Name: "deny", Target: "0f3a"}},
		{argv: []string{"approve"}, wantErr: true},
		{argv: []string{"deny", "1", "2"}, wantErr: true},
		{argv: []string{"revoke", "a", "b"}, wantErr: true},
		{argv: []string{"remote", "status"}, want: invocation{Name: "remote", RemoteVerb: "status"}},
		{argv: []string{"remote", "logout"}, want: invocation{Name: "remote", RemoteVerb: "logout"}},
		{argv: []string{"remote", "on"}, want: invocation{Name: "remote", RemoteVerb: "on"}},
		{argv: []string{"remote", "off"}, want: invocation{Name: "remote", RemoteVerb: "off"}},
		{argv: []string{"remote"}, wantErr: true},
		{argv: []string{"remote", "sideways"}, wantErr: true},
		{argv: []string{"make-resumable", "sess-1"}, want: invocation{Name: "make-resumable", Target: "sess-1"}},
		{argv: []string{"undo-make-resumable", "run-9"}, want: invocation{Name: "undo-make-resumable", Target: "run-9"}},
		{argv: []string{"make-resumable"}, wantErr: true},
		{argv: []string{"help"}, want: invocation{Name: "help"}},
		{argv: []string{"--help"}, want: invocation{Name: "help"}},
		{argv: []string{"pair", "-h"}, want: invocation{Name: "help"}},
		{argv: []string{"statsu"}, wantErr: true},
		{argv: []string{"login"}, wantErr: true},
	}
	for _, tc := range cases {
		got, err := parseCommand(tc.argv, getenv)
		if tc.wantErr {
			if err == nil || !errors.Is(err, errUsage) {
				t.Errorf("%q: err = %v, want a usage error (got %+v)", tc.argv, err, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: unexpected error %v", tc.argv, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%q: got %+v, want %+v", tc.argv, got, tc.want)
		}
	}
}

func TestParseCommandServeDefaultsComeFromEnv(t *testing.T) {
	env := map[string]string{"AF_ADDR": "127.0.0.1:5000", "AF_DB": "/data/af.db"}
	got, err := parseCommand([]string{"serve"}, func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if got.Addr != "127.0.0.1:5000" || got.DB != "/data/af.db" {
		t.Fatalf("got %+v", got)
	}
	got, _ = parseCommand(nil, func(k string) string { return env[k] })
	if got.Addr != "127.0.0.1:5000" || got.DB != "/data/af.db" {
		t.Fatalf("empty argv got %+v", got)
	}
	got, _ = parseCommand([]string{"serve", "--addr", "127.0.0.1:1"}, func(k string) string { return env[k] })
	if got.Addr != "127.0.0.1:1" {
		t.Fatalf("flag did not override env: %+v", got)
	}
}

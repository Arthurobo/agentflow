package agentapi

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"time"
)

// Check is one doctor probe result.
type Check struct {
	Name string `json:"name"`
	OK   bool   `json:"ok"`
	Info string `json:"info,omitempty"`
}

// DoctorOptions configures the doctor.
type DoctorOptions struct {
	// Extras allows the caller to append checks (Tailscale etc.).
	Extras []Check
}

func (s *Server) doctorChecks(ctx context.Context) []Check {
	return RunDoctor(ctx, DoctorOptions{Extras: s.doctorExtras})
}

// RunDoctor performs the checks and returns them in order.
func RunDoctor(ctx context.Context, opts DoctorOptions) []Check {
	checks := []Check{}
	fail := func(name, info string) { checks = append(checks, Check{Name: name, OK: false, Info: info}) }
	pass := func(name, info string) { checks = append(checks, Check{Name: name, OK: true, Info: info}) }

	// 1. claude binary + version
	var claudeVersion string
	bin := ""
	if p, err := exec.LookPath("claude"); err == nil {
		bin = p
	}
	if bin != "" {
		ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
		out, err := exec.CommandContext(ctx2, bin, "--version").Output() //nolint:gosec
		cancel()
		if err == nil {
			claudeVersion = string(out)
		}
	}
	if bin != "" && claudeVersion != "" {
		pass("claude binary", fmt.Sprintf("%s (%s)",
			filepath.Base(bin), firstLine(claudeVersion)))
	} else if bin != "" {
		fail("claude binary", "found but --version failed")
	} else {
		fail("claude binary", "not found in PATH — claude is required")
	}

	// The daemon appends its remote-access check here.
	if len(opts.Extras) > 0 {
		checks = append(checks, opts.Extras...)
	}
	return checks
}

func firstLine(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			return s[:i]
		}
	}
	return s
}

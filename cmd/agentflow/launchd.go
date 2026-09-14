package main

import (
	"context"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// launchdLabel identifies the LaunchAgent; the plist file is <label>.plist.
const launchdLabel = "com.arthurobo.agentflow"

// launchdService drives a per-user LaunchAgent.
type launchdService struct {
	home string
	uid  string
	run  runner
}

func (s *launchdService) Supported() bool { return true }

func (s *launchdService) plistPath() string {
	return filepath.Join(s.home, "Library", "LaunchAgents", launchdLabel+".plist")
}

func (s *launchdService) logDir() string {
	return filepath.Join(s.home, "Library", "Logs", "agentflow")
}

func (s *launchdService) domain() string  { return "gui/" + s.uid }
func (s *launchdService) service() string { return s.domain() + "/" + launchdLabel }

func (s *launchdService) launchctl(ctx context.Context, args ...string) (string, error) {
	out, err := s.run(ctx, "launchctl", args...)
	if err != nil {
		return out, fmt.Errorf("launchctl %s: %w (%s)", strings.Join(args, " "), err, trimOut(out))
	}
	return out, nil
}

func xmlEscape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// renderLaunchdPlist builds the LaunchAgent. launchd expands nothing, so every
// path is absolute.
func renderLaunchdPlist(spec serviceSpec, home string) string {
	logDir := filepath.Join(home, "Library", "Logs", "agentflow")
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + launchdLabel + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>` + xmlEscape(spec.ExecPath) + `</string>
		<string>serve</string>
	</array>
	<key>EnvironmentVariables</key>
	<dict>
		<key>PATH</key>
		<string>` + xmlEscape(spec.PATH) + `</string>
	</dict>
	<key>WorkingDirectory</key>
	<string>` + xmlEscape(home) + `</string>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>ExitTimeOut</key>
	<integer>20</integer>
	<key>StandardOutPath</key>
	<string>` + xmlEscape(filepath.Join(logDir, "agentflow.log")) + `</string>
	<key>StandardErrorPath</key>
	<string>` + xmlEscape(filepath.Join(logDir, "agentflow.log")) + `</string>
</dict>
</plist>
`
}

func (s *launchdService) loaded(ctx context.Context) bool {
	_, err := s.run(ctx, "launchctl", "print", s.service())
	return err == nil
}

func (s *launchdService) Install(ctx context.Context, spec serviceSpec) (bool, error) {
	plist := renderLaunchdPlist(spec, s.home)
	path := s.plistPath()
	old, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	changed := string(old) != plist
	if err := os.MkdirAll(s.logDir(), 0o700); err != nil {
		return false, fmt.Errorf("create %s: %w", s.logDir(), err)
	}
	if changed {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return false, err
		}
		if err := writeFileAtomic(path, []byte(plist), 0o644); err != nil {
			return false, fmt.Errorf("write %s: %w", path, err)
		}
	}
	// A previous `agentflow stop` disabled the agent; bootstrap refuses a
	// disabled service.
	if _, err := s.launchctl(ctx, "enable", s.service()); err != nil {
		return changed, err
	}
	loaded := s.loaded(ctx)
	switch {
	case loaded && !changed:
		_, err = s.launchctl(ctx, "kickstart", s.service())
	case loaded && changed:
		// launchd keeps the definition it loaded; reload it.
		if _, err = s.launchctl(ctx, "bootout", s.service()); err == nil {
			_, err = s.launchctl(ctx, "bootstrap", s.domain(), path)
		}
	default:
		_, err = s.launchctl(ctx, "bootstrap", s.domain(), path)
	}
	return changed, err
}

func (s *launchdService) Stop(ctx context.Context) error {
	if s.loaded(ctx) {
		if _, err := s.launchctl(ctx, "bootout", s.service()); err != nil {
			return err
		}
	}
	// Keep it from coming back at the next login.
	_, err := s.launchctl(ctx, "disable", s.service())
	return err
}

func (s *launchdService) Restart(ctx context.Context) error {
	_, err := s.launchctl(ctx, "kickstart", "-k", s.service())
	return err
}

func (s *launchdService) Remove(ctx context.Context) error {
	if s.loaded(ctx) {
		if _, err := s.launchctl(ctx, "bootout", s.service()); err != nil {
			return err
		}
	}
	if err := os.Remove(s.plistPath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (s *launchdService) Query(ctx context.Context) serviceState {
	var st serviceState
	if _, err := os.Stat(s.plistPath()); err == nil {
		st.Installed = true
	}
	out, err := s.run(ctx, "launchctl", "print", s.service())
	if err != nil {
		return st
	}
	st.Enabled = true
	st.Running, st.PID = parseLaunchctlPrint(out)
	return st
}

// parseLaunchctlPrint reads "state = running" and "pid = N" from
// `launchctl print` output.
func parseLaunchctlPrint(out string) (running bool, pid int) {
	for _, line := range strings.Split(out, "\n") {
		l := strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(l, "state = "); ok {
			running = v == "running"
		}
		if v, ok := strings.CutPrefix(l, "pid = "); ok {
			if n, err := strconv.Atoi(v); err == nil {
				pid = n
			}
		}
	}
	return running, pid
}

// ServicePATH reads PATH back out of the installed plist.
func (s *launchdService) ServicePATH() string {
	data, err := os.ReadFile(s.plistPath())
	if err != nil {
		return ""
	}
	return plistPATH(data)
}

// plistPATH extracts the PATH environment variable from a plist: the string
// that follows <key>PATH</key>. Only EnvironmentVariables uses that key.
func plistPATH(data []byte) string {
	dec := xml.NewDecoder(strings.NewReader(string(data)))
	dec.Strict = false
	wantValue := false
	for {
		tok, err := dec.Token()
		if err != nil {
			return ""
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch start.Name.Local {
		case "key":
			var k string
			if err := dec.DecodeElement(&k, &start); err != nil {
				return ""
			}
			wantValue = k == "PATH"
		case "string":
			var v string
			if err := dec.DecodeElement(&v, &start); err != nil {
				return ""
			}
			if wantValue {
				return v
			}
		default:
			wantValue = false
		}
	}
}

func (s *launchdService) LogHint() string {
	return filepath.Join(s.logDir(), "agentflow.log")
}

// StaysUpAfterLogout: a LaunchAgent runs for the logged-in GUI session; there
// is no linger switch to suggest.
func (s *launchdService) StaysUpAfterLogout(context.Context) bool { return true }

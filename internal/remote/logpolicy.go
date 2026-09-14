package remote

// noLogsEnv is the environment knob that stops tsnet uploading client logs to
// Tailscale.
//
// Verified in tailscale.com@v1.102.4: tsnet/tsnet.go:1085 (startLogger) builds
// the log uploader's HTTP client with logpolicy.NewLogtailTransport, and
// logpolicy/logpolicy.go:892 (TransportOptions.New) returns a transport that
// never touches the network when envknob.NoLogsNoSupport() is true, which is
// this variable (envknob/envknob.go:483). hostinfo/hostinfo.go:66 also reports
// the opt-out to the control server. The variable is read when the node
// starts, so it has to be set before tsnet.Server.Start.
const noLogsEnv = "TS_NO_LOGS_NO_SUPPORT"

// applyLogPolicy disables Tailscale log upload unless the user opted in with
// AF_TS_LOGS=on. With the opt-in, an inherited value is left alone.
func applyLogPolicy(logsOn bool, setenv func(key, value string) error) error {
	if logsOn {
		return nil
	}
	return setenv(noLogsEnv, "true")
}

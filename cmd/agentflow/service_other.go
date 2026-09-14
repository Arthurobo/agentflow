//go:build !linux && !darwin

package main

// Everywhere else the daemon still runs in the foreground with
// `agentflow serve`; only the background lifecycle commands are unavailable,
// and they say so.
func newServiceManager() serviceManager { return unsupportedService{} }

//go:build linux

package main

import "os"

func newServiceManager() serviceManager {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return unsupportedService{}
	}
	return &systemdService{home: home, run: execRunner}
}

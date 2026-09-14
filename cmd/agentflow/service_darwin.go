//go:build darwin

package main

import (
	"os"
	"strconv"
)

func newServiceManager() serviceManager {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return unsupportedService{}
	}
	return &launchdService{home: home, uid: strconv.Itoa(os.Getuid()), run: execRunner}
}

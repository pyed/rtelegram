//go:build !windows

package main

import (
	"errors"
	"os"
)

func isAdmin() bool {
	return os.Geteuid() == 0
}

func installWindowsService(string, []string) error {
	return errors.New("not Windows")
}

func uninstallWindowsService() error {
	return errors.New("not Windows")
}

// runAsService reports false: only Windows starts rtelegram as a service
// that must be talked to.
func runAsService() bool {
	return false
}

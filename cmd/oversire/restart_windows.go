//go:build windows

package main

import (
	"os"
	"os/exec"
)

// restart starts the new binary, then this process exits. A logon task does not restart a stopped runner.
func restart(exe string) error {
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Start()
}

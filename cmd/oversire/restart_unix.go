//go:build !windows

package main

import (
	"os"
	"syscall"
)

// restart replaces this process with the new binary. The pid stays, so the service sees no exit.
func restart(exe string) error {
	return syscall.Exec(exe, os.Args, os.Environ())
}

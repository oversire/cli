// Package proc runs child processes. The other packages get it as a small interface.
package proc

import (
	"bytes"
	"context"
	"os/exec"
)

// Local runs commands on this machine.
type Local struct{}

// Run starts exe with args, writes stdin to it and waits. stdout and stderr come back also on an error.
func (Local) Run(ctx context.Context, exe string, args []string, stdin []byte) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, exe, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	return out.Bytes(), errOut.Bytes(), err
}

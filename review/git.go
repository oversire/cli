package review

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// Runner runs one child process. proc.Local is the real one.
type Runner interface {
	Run(ctx context.Context, exe string, args []string, stdin []byte) (stdout, stderr []byte, err error)
}

// Git runs git in a folder through a Runner.
type Git struct{ run Runner }

func NewGit(r Runner) Git { return Git{run: r} }

// Run gives the stdout of git without the last line end. A failure has the stderr text.
func (g Git) Run(ctx context.Context, dir string, args ...string) (string, error) {
	out, _, err := g.Exit(ctx, dir, args...)
	return out, err
}

// Exit is Run that also gives the exit code. A git that exits with a code > 0 gives the code and an error.
func (g Git) Exit(ctx context.Context, dir string, args ...string) (string, int, error) {
	out, errOut, err := g.run.Run(ctx, "git", append([]string{"-C", dir}, args...), nil)
	if err == nil {
		return strings.TrimRight(string(out), "\n"), 0, nil
	}
	code := -1
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code = exit.ExitCode()
	}
	text := strings.TrimSpace(string(errOut))
	if text == "" {
		text = err.Error()
	}
	return strings.TrimRight(string(out), "\n"), code, fmt.Errorf("git %s: %s", args[0], text)
}

// Changed gives the paths that the worktree dir changed since it left the HEAD of repo:
// the diff from the merge base to the work tree (deletes and both names of a rename included), plus untracked files.
func (g Git) Changed(ctx context.Context, repo, dir string) ([]string, error) {
	head, err := g.Run(ctx, repo, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	base, err := g.Run(ctx, dir, "merge-base", head, "HEAD")
	if err != nil {
		return nil, err
	}
	diff, err := g.Run(ctx, dir, "diff", "--name-only", "--no-renames", base)
	if err != nil {
		return nil, err
	}
	untracked, err := g.Run(ctx, dir, "ls-files", "--others", "--exclude-standard")
	if err != nil {
		return nil, err
	}
	return lines(diff + "\n" + untracked), nil
}

// lines gives the lines of s that are not blank, trimmed.
func lines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

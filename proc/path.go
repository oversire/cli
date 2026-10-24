package proc

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const pathMark = "__OVERSIRE_PATH__"

// LoginPath gives the PATH of an interactive login shell, or "" on a failure.
// launchd starts the daemon with a short PATH. Agent CLIs such as claude are often under nvm or Homebrew.
// rc files can print text, so the value sits between two marks.
func LoginPath(ctx context.Context, shell string) string {
	if shell == "" {
		shell = "/bin/zsh"
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, shell, "-l", "-i", "-c", `printf '`+pathMark+`%s`+pathMark+`' "$PATH"`)
	cmd.WaitDelay = time.Second
	out, _ := cmd.Output()
	parts := strings.Split(string(out), pathMark)
	if len(parts) < 3 {
		return ""
	}
	return parts[len(parts)-2]
}

// MergePath puts the folders of first before the other folders of second, with no repeats.
func MergePath(first, second string) string {
	var dirs []string
	seen := map[string]bool{}
	for _, d := range filepath.SplitList(first + string(filepath.ListSeparator) + second) {
		if d != "" && !seen[d] {
			seen[d] = true
			dirs = append(dirs, d)
		}
	}
	return strings.Join(dirs, string(filepath.ListSeparator))
}

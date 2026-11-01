package review

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/oversire/cli/wire"
)

// ReadyCommand is the hand-off command of the agent, without the note.
func ReadyCommand(kind wire.ReviewKind, key string) string {
	if kind == wire.ReviewPlan {
		return "oversire review ready --key " + key + " --kind plan --plan <file.md>"
	}
	return "oversire review ready --key " + key
}

// ReplyCommand is the command that answers a comment, without the id and the text.
func ReplyCommand(key string) string { return "oversire review reply --key " + key }

// prompt is the text of a round that requests changes: the header, the summary, each comment, then the rules.
// dir is the group folder of a code review, or the folder of the plan.
func prompt(kind wire.ReviewKind, key, dir string, round wire.ReviewRound, cs []wire.ReviewComment) string {
	var b strings.Builder
	what := key
	if kind == wire.ReviewPlan {
		what = "the plan of " + key
	}
	fmt.Fprintf(&b, "Review of %s, round %d: changes requested.\n", what, round.N)
	if s := strings.TrimSpace(round.Summary); s != "" {
		fmt.Fprintf(&b, "\n%s\n", s)
	}
	for _, c := range cs {
		fmt.Fprintf(&b, "\n[%s] %s\n", c.ID, anchorText(c))
		if kind == wire.ReviewCode && c.Excerpt != "" {
			for _, l := range strings.Split(c.Excerpt, "\n") {
				fmt.Fprintf(&b, "    > %s\n", l)
			}
		}
		fmt.Fprintf(&b, "%s\n", strings.TrimSpace(c.Text))
	}
	b.WriteString("\n")
	if kind == wire.ReviewPlan {
		fmt.Fprintf(&b, "Do not write code. Change the plan in %s for each comment.\n", dir)
	} else {
		fmt.Fprintf(&b, "Work in the group folder %s. Do not merge.\n", dir)
		b.WriteString("Make the change for each comment and commit it to the branch of the repo.\n")
	}
	if len(cs) > 0 {
		fmt.Fprintf(&b, "Then answer each comment with its id:\n  %s <id> \"<what you did>\"\n", ReplyCommand(key))
	}
	if kind == wire.ReviewCode {
		b.WriteString("Run the tests.\n")
	}
	fmt.Fprintf(&b, "At the end, hand off for review:\n  %s --note \"<one line>\"\n", ReadyCommand(kind, key))
	return b.String()
}

// anchorText is repo/path:from-line for code, with "(old)" for a line of the base, or the block heading of a plan.
func anchorText(c wire.ReviewComment) string {
	a := c.Anchor
	if a.IsPlan() {
		h := c.Excerpt
		if h == "" {
			h = a.Block
		}
		return fmt.Sprintf("Block %q", h)
	}
	lines := fmt.Sprint(a.Line)
	if a.From > 0 && a.From < a.Line {
		lines = fmt.Sprintf("%d-%d", a.From, a.Line)
	}
	s := fmt.Sprintf("%s:%s", filepath.ToSlash(filepath.Join(a.Repo, a.Path)), lines)
	if a.Side == "old" {
		s += " (old)"
	}
	return s
}

// fixPrompt asks the agent to rebase the group on its bases and fix the conflicts.
func fixPrompt(key, dir string, repos []wire.ReviewRepo) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The merge of %s stops on a conflict.\n\n", key)
	fmt.Fprintf(&b, "Work in the group folder %s. Do not merge.\n", dir)
	b.WriteString("Rebase the branch of each repo on its base, fix the conflicts, and run the tests:\n")
	for _, r := range repos {
		line := fmt.Sprintf("  %s: rebase %s on %s", r.Name, r.Branch, r.Base)
		if len(r.ConflictFiles) > 0 {
			line += " (conflicts: " + strings.Join(r.ConflictFiles, ", ") + ")"
		}
		b.WriteString(line + "\n")
	}
	fmt.Fprintf(&b, "At the end, hand off for review:\n  %s --note \"<one line>\"\n", ReadyCommand(wire.ReviewCode, key))
	return b.String()
}

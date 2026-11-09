package worker

import "strings"

// Prompt is the first prompt of a worker: the plan and the fixed worker contract.
type Prompt struct{ Key, Plan string }

func (p Prompt) String() string {
	var b strings.Builder
	if plan := strings.TrimSpace(p.Plan); plan != "" {
		b.WriteString(plan)
		b.WriteString("\n\n---\n\n")
	}
	b.WriteString("You are the worker for " + p.Key + ". An overseer reads your work and answers you.\n")
	b.WriteString("- Stay in the worktree group of this folder. Do not change files outside it.\n")
	b.WriteString("- Run the tests of each repository that you change.\n")
	b.WriteString("- Do not push. Do not merge. Do not open a pull request.\n")
	b.WriteString("- When the work is finished, or when you cannot go on, run:\n")
	b.WriteString("  oversire worker done --verdict done|blocked --note \"<one short line>\"\n")
	b.WriteString("- Then stop. Wait for the overseer.\n")
	return b.String()
}

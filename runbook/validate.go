// Package runbook runs runbooks: a fixed list of steps on one item, one agent session for each step.
package runbook

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/oversire/cli/wire"
)

// MaxRounds is the highest round limit of a review.
const MaxRounds = 5

// Problem is one rule that a runbook breaks. Step is empty for a problem of the full runbook.
type Problem struct {
	Step string `json:"step"`
	Text string `json:"text"`
}

var (
	stepID      = regexp.MustCompile(`^[a-z0-9-]+$`)
	placeholder = regexp.MustCompile(`\{([a-z]+)\}`)
	known       = []string{"key", "url", "title", "item", "workspace", "project", "round", "notes", "worktree", "done"}
)

// Validate checks the rules of the app (Runbook.validate), but not the agent list.
func Validate(b wire.Runbook) []Problem {
	var out []Problem
	add := func(step, format string, a ...any) { out = append(out, Problem{Step: step, Text: fmt.Sprintf(format, a...)}) }
	if strings.TrimSpace(b.Name) == "" {
		add("", "The runbook needs a name.")
	}
	if len(b.Steps) == 0 {
		add("", "The runbook needs at least one step.")
	}
	seen := map[string]bool{}
	fixes := map[string]string{} // fix step to its review
	for i, s := range b.Steps {
		switch {
		case !stepID.MatchString(s.ID):
			add(s.ID, "The step id %q is not valid. Use only a-z, 0-9 and -.", s.ID)
		case seen[s.ID]:
			add(s.ID, "Two steps have the id %q.", s.ID)
		}
		seen[s.ID] = true
		if !strings.Contains(s.Prompt, "{done}") {
			add(s.ID, "The prompt has no {done}. The agent needs it to end the step.")
		}
		for _, m := range placeholder.FindAllStringSubmatch(s.Prompt, -1) {
			if !slices.Contains(known, m[1]) {
				add(s.ID, "The prompt has the unknown placeholder {%s}.", m[1])
			}
		}
		if i == 0 && s.PauseBefore {
			add(s.ID, "The first step cannot pause before it starts.")
		}
		if s.Review == nil {
			continue
		}
		if s.Review.Rounds < 0 || s.Review.Rounds > MaxRounds {
			add(s.ID, "The rounds of a review must be 0 to %d.", MaxRounds)
		}
		fix := b.Index(s.Review.Fix)
		switch {
		case fix <= i:
			add(s.ID, "The fix step %q must be a later step.", s.Review.Fix)
		case b.Steps[fix].Review != nil:
			add(s.ID, "The fix step %q cannot be a review.", s.Review.Fix)
		case fixes[s.Review.Fix] != "":
			add(s.ID, "The fix step %q is also the fix step of %q.", s.Review.Fix, fixes[s.Review.Fix])
		default:
			fixes[s.Review.Fix] = s.ID
		}
	}
	return out
}

// validateStart checks the start: the runbook, the item, the repo, the start step and the agent of each step.
func validateStart(s wire.RunbookStart) []Problem {
	out := Validate(s.Runbook)
	if s.Item.Key == "" {
		out = append(out, Problem{Text: "The start needs an item with a key."})
	}
	if s.Repo == "" {
		out = append(out, Problem{Text: "The start needs a repo."})
	}
	if s.StartAt != "" && s.Runbook.Index(s.StartAt) < 0 {
		out = append(out, Problem{Step: s.StartAt, Text: fmt.Sprintf("The runbook has no step %q to start at.", s.StartAt)})
	}
	for _, st := range s.Runbook.Steps {
		if _, ok := s.Agents[st.Agent]; !ok {
			out = append(out, Problem{Step: st.ID, Text: fmt.Sprintf("The agent %q is not in the agents of the start.", st.Agent)})
		}
	}
	return out
}

// fixOf gives the review whose fix step is id, else nil.
func fixOf(b wire.Runbook, id string) *wire.RunbookStep {
	for i := range b.Steps {
		if r := b.Steps[i].Review; r != nil && r.Fix == id {
			return &b.Steps[i]
		}
	}
	return nil
}

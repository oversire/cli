package runbook

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/oversire/cli/wire"
)

// DoneCommand is the command that ends a step, without the verdict and the note.
func DoneCommand(run, step string) string {
	return "oversire step done --run " + run + " --step " + step
}

// render fills the placeholders of the prompt of step for round. An unknown placeholder stays as typed.
func render(r *wire.RunbookRun, step wire.RunbookStep, round int) string {
	return placeholder.ReplaceAllStringFunc(step.Prompt, func(m string) string {
		switch m[1 : len(m)-1] {
		case "key":
			return r.Item.Key
		case "url":
			return r.Item.URL
		case "title":
			if r.Title == "" {
				return r.Item.Key
			}
			return r.Title
		case "item":
			return r.Item.ID
		case "workspace":
			return r.Item.Project.Account.ID
		case "project":
			return r.Item.Project.Key
		case "round":
			return strconv.Itoa(round)
		case "notes":
			return notes(r)
		case "worktree":
			return ".wt/" + strings.ToLower(r.Item.Key)
		case "done":
			return DoneCommand(r.ID, step.ID)
		}
		return m
	})
}

// notes is the notes of the start, then one part for each finished step round with a note.
func notes(r *wire.RunbookRun) string {
	var parts []string
	if r.Notes != "" {
		parts = append(parts, r.Notes)
	}
	for _, sr := range r.Steps {
		if sr.Note == "" || sr.Ended == nil {
			continue
		}
		name := sr.Step
		if s := r.Runbook.Step(sr.Step); s != nil {
			name = s.Name
		}
		if sr.Round > 1 {
			name = fmt.Sprintf("%s (round %d)", name, sr.Round)
		}
		parts = append(parts, name+": "+sr.Note)
	}
	return strings.Join(parts, "; ")
}

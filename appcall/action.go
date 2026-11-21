package appcall

import (
	"encoding/json"
	"regexp"
	"strings"
)

// Action is an action that a relay sends to an app, from an app call or a member call.
type Action struct {
	Name string
	Args json.RawMessage
}

// readActions are the actions that change nothing. Any other action counts as a change.
// Keep it the same as the actions of kind read in the app catalog that the CLI or a client app sends.
var readActions = map[string]bool{
	"item.get": true, "item.list": true, "item.markdown": true,
	"epic.list": true, "epic.show": true, "epic.plan": true,
	"project.northStar": true, "project.statuses": true, "project.labels": true, "project.people": true,
	"search.all":  true,
	"mirror.read": true, "people.get": true, "people.list": true, "question.list": true,
	"queue.list": true, "run.explain": true, "run.log": true,
	"runbook.get": true, "runbook.list": true, "runbook.status": true, "runbook.validate": true,
	"work.context": true,
}

var safeWord = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

var shellQuoter = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", `\$`, "`", "\\`")

// Changes tells whether the action can change data. An unknown action counts as a change.
func (a Action) Changes() bool {
	return !readActions[a.Name]
}

// TimeoutMessage is the 504 text. who is the subject of the sentence. A change can be made after
// the timeout, so the text tells the caller to check first.
func (a Action) TimeoutMessage(who string) string {
	if !a.Changes() {
		return who + " did not answer in time. Try again."
	}
	return who + " did not answer in time. The change can be made already. " +
		"Check it before you try again, for example with: " + a.checkHint() + "."
}

func (a Action) checkHint() string {
	var args struct {
		Project string `json:"project"`
		Title   string `json:"title"`
		Key     string `json:"key"`
	}
	_ = json.Unmarshal(a.Args, &args)
	project := ""
	if safeWord.MatchString(args.Project) {
		project = " --project " + args.Project
	}
	switch {
	case a.Name == "item.create":
		title := strings.Join(strings.Fields(args.Title), " ")
		if title == "" {
			return "oversire item list" + project
		}
		return "oversire item list" + project + ` --text "` + shellQuoter.Replace(title) + `"`
	case a.Name == "epic.create":
		return "oversire epic list" + project
	case safeWord.MatchString(args.Key):
		return "oversire item show " + args.Key
	}
	return "oversire item list"
}

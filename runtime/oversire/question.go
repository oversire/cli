package oversire

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	agentwire "github.com/trebi-ai/agent-wire"

	"github.com/oversire/cli/wire"
)

// askTool is the Claude Code tool that asks the user. agent-wire sends it as a permission request.
const askTool = "AskUserQuestion"

// ErrAnswered: the question of the answer is not open. The user answered it already, or the agent went on.
var ErrAnswered = errors.New("already answered")

// askInput is the input of AskUserQuestion.
type askInput struct {
	Questions []struct {
		Question string `json:"question"`
		Header   string `json:"header"`
		Options  []struct {
			Label       string `json:"label"`
			Description string `json:"description"`
		} `json:"options"`
		MultiSelect bool `json:"multiSelect"`
	} `json:"questions"`
}

type openQuestion struct {
	question wire.RunnerQuestion
	texts    []string // the question text of each item, the keys of the answers
	input    map[string]any
}

// parseAsk reads an AskUserQuestion request. The id is the pending request id of agent-wire.
func parseAsk(p *agentwire.Permission, round int, at time.Time) (*openQuestion, error) {
	var in askInput
	var raw map[string]any
	if err := json.Unmarshal([]byte(p.Input), &in); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(p.Input), &raw); err != nil {
		return nil, err
	}
	if len(in.Questions) == 0 {
		return nil, errors.New("no questions")
	}
	q := wire.RunnerQuestion{ID: p.ID, Round: round, AskedAt: at}
	oq := &openQuestion{input: raw}
	for i, x := range in.Questions {
		item := wire.RunnerQuestionItem{
			ID: fmt.Sprintf("q%d", i+1), Header: x.Header, Text: clip(x.Question, 2000), Kind: wire.QuestionSingle,
			Options: []wire.QuestionOption{}, AllowsOther: true,
		}
		if x.MultiSelect {
			item.Kind = wire.QuestionMulti
		}
		if len(x.Options) == 0 {
			item.Kind, item.AllowsOther = wire.QuestionFree, false
		}
		for _, o := range x.Options {
			item.Options = append(item.Options, wire.QuestionOption{Label: o.Label, Detail: o.Description})
		}
		q.Items = append(q.Items, item)
		oq.texts = append(oq.texts, x.Question)
	}
	oq.question = q
	return oq, nil
}

// askQuestion shows an AskUserQuestion request as the question of the session. It gives false when the
// input does not parse; the caller then shows it as a permission. Call it with s.mu held.
func (s *session) askQuestion(p *agentwire.Permission) bool {
	s.rounds++
	oq, err := parseAsk(p, s.rounds, s.rt.now().UTC())
	if err != nil {
		s.rounds--
		return false
	}
	s.question = oq
	s.info.Question = &oq.question
	for _, it := range oq.question.Items {
		s.log.add("? " + it.Text)
	}
	if len(s.permissions) == 0 {
		s.change(wire.SessionWaitingInput)
	} else {
		s.publish()
	}
	return true
}

// answerQuestion gives the answers to the agent as the updated input of AskUserQuestion.
func (s *session) answerQuestion(ctx context.Context, a wire.QuestionAnswer) error {
	s.mu.Lock()
	oq := s.question
	if oq == nil || oq.question.ID != a.ID {
		s.mu.Unlock()
		return fmt.Errorf("The session %s has no open question %s: %w.", s.id, a.ID, ErrAnswered)
	}
	answers := map[string]any{}
	for i, it := range oq.question.Items {
		for _, x := range a.Items {
			if x.ID != it.ID {
				continue
			}
			parts := append([]string{}, x.Choices...)
			if other := strings.TrimSpace(x.Other); other != "" {
				parts = append(parts, other)
			}
			answers[oq.texts[i]] = strings.Join(parts, ", ")
		}
	}
	input := map[string]any{}
	for k, v := range oq.input {
		input[k] = v
	}
	input["answers"] = answers
	// The tool has no field for one note. It goes with the first question, as the notes of the form.
	if a.Note != "" && len(oq.texts) > 0 {
		input["annotations"] = map[string]any{oq.texts[0]: map[string]any{"notes": a.Note}}
	}
	updated, err := json.Marshal(input)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	s.clearQuestion()
	s.log.add("= " + a.ID + " answered")
	aw := s.aw
	s.mu.Unlock()
	return aw.AnswerPermission(ctx, a.ID, agentwire.Decision{Allow: true, UpdatedInput: updated})
}

// clearQuestion drops the open question and goes on working. Call it with s.mu held.
func (s *session) clearQuestion() {
	if s.question == nil {
		return
	}
	s.question = nil
	s.info.Question = nil
	if len(s.permissions) == 0 {
		s.change(wire.SessionWorking)
	} else {
		s.publish()
	}
}

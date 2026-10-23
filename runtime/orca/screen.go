package orca

import (
	"context"
	"strconv"
	"strings"
	"time"
)

// Screen reads and types into the terminals of the Orca on this machine. It implements questions.Screen and questions.Searcher.
type Screen struct{ o *Orca }

func NewScreen(o *Orca) *Screen { return &Screen{o: o} }

// Read gives the last rows of the terminal as they show, with the blank rows and the rules.
func (s *Screen) Read(ctx context.Context, terminal string, lines int) ([]string, error) {
	var r struct {
		Terminal struct {
			Tail []string `json:"tail"`
		} `json:"terminal"`
	}
	if err := s.o.callInto(ctx, 20*time.Second, &r, "terminal", "read", "--terminal", terminal, "--limit", strconv.Itoa(lines)); err != nil {
		return nil, err
	}
	out := make([]string, len(r.Terminal.Tail))
	for i, l := range r.Terminal.Tail {
		out[i] = strings.ReplaceAll(l, " ", " ")
	}
	return out, nil
}

// Send types the text. With enter it submits the text as a prompt and waits until the agent takes it.
func (s *Screen) Send(ctx context.Context, terminal, text string, enter bool) error {
	if enter {
		_, err := s.o.call(ctx, 30*time.Second, "terminal", "send", "--terminal", terminal, "--text", text, "--enter", "--wait-submit", "15")
		return err
	}
	_, err := s.o.call(ctx, 10*time.Second, "terminal", "send", "--terminal", terminal, "--text", text)
	return err
}

// Search gives the transcript files of the Claude sessions in cwd where orca search finds the words, newest first.
func (s *Screen) Search(ctx context.Context, cwd, words string) ([]string, error) {
	var r struct {
		Hits []struct {
			Source struct {
				FilePath string `json:"filePath"`
			} `json:"source"`
		} `json:"hits"`
	}
	if err := s.o.callInto(ctx, 30*time.Second, &r, "search", "--query", words, "--agent", "claude", "--path", cwd,
		"--scope", "conversation", "--sort", "newest", "--limit", "5"); err != nil {
		return nil, err
	}
	var out []string
	for _, h := range r.Hits {
		if h.Source.FilePath != "" {
			out = append(out, h.Source.FilePath)
		}
	}
	return out, nil
}

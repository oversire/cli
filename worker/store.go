package worker

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Record is what the daemon keeps for one worker session. Key is the start key, for example work:OVERSIRE-12.
type Record struct {
	Session    string         `json:"session"`
	Key        string         `json:"key,omitempty"`
	Cursors    map[string]int `json:"cursors,omitempty"`
	UpdatedAt  time.Time      `json:"updatedAt"`
	Overseer   string         `json:"overseer,omitempty"` // the session id of the overseer
	Harness    string         `json:"harness,omitempty"`
	Model      string         `json:"model,omitempty"`
	Dir        string         `json:"dir,omitempty"`
	Plan       string         `json:"plan,omitempty"`
	State      State          `json:"state,omitempty"`
	Verdict    string         `json:"verdict,omitempty"` // the last done or blocked report
	Note       string         `json:"note,omitempty"`
	Budget     time.Duration  `json:"budget,omitempty"` // the time budget; each answer to a budget escalation adds DefaultBudget
	StartedAt  time.Time      `json:"startedAt,omitzero"`
	LastAction time.Time      `json:"lastAction,omitzero"` // the last action of the overseer on the worker
	Escalation *Escalation    `json:"escalation,omitempty"`
}

// Item is the item key of the worker: the start key without its work: prefix.
func (r Record) Item() string { return strings.TrimPrefix(r.Key, "work:") }

// State is the state of a worker that the daemon keeps. The runtime state stays on the session.
type State string

const (
	StateWorking       State = "working"
	StateAwaitingCheck State = "awaitingCheck" // the worker reported done
	StateBlocked       State = "blocked"       // the worker reported blocked
	StateStopped       State = "stopped"
)

// DefaultBudget is the time budget of a worker. A budget answer of the user adds it again.
const DefaultBudget = 3 * time.Hour

// EscalationReason tells why a worker asks the user.
type EscalationReason string

const (
	ReasonOverseer EscalationReason = "overseer"
	ReasonFallback EscalationReason = "fallback" // the overseer did no action on an ask for too long
	ReasonBudget   EscalationReason = "budget"
)

// Escalation is an open ask to the user. An empty ID is the open ask of the worker itself.
type Escalation struct {
	ID       string           `json:"id,omitempty"`
	Question string           `json:"question,omitempty"`
	Reason   EscalationReason `json:"reason"`
	At       time.Time        `json:"at"`
}

// Store keeps one JSON file for each worker session in its folder: <dir>/<session>.json.
type Store struct{ dir string }

func NewStore(dir string) *Store { return &Store{dir: dir} }

func (s *Store) path(session string) (string, error) {
	if session == "" || session != filepath.Base(session) || strings.HasPrefix(session, ".") {
		return "", fmt.Errorf("bad session id %q", session)
	}
	return filepath.Join(s.dir, session+".json"), nil
}

// Load reads the record of the session. A file that is not there gives an empty Record.
func (s *Store) Load(session string) (Record, error) {
	path, err := s.path(session)
	if err != nil {
		return Record{}, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Record{}, nil
	}
	if err != nil {
		return Record{}, err
	}
	var r Record
	if err := json.Unmarshal(data, &r); err != nil {
		return Record{}, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return r, nil
}

// Save writes the record to a temp file, then renames it, so a crash never leaves half a file.
func (s *Store) Save(r Record) error {
	path, err := s.path(r.Session)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// List gives all records. A bad file is skipped.
func (s *Store) List() ([]Record, error) {
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Record
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || e.IsDir() {
			continue
		}
		if r, err := s.Load(name); err == nil && r.Session != "" {
			out = append(out, r)
		}
	}
	return out, nil
}

// ByKey gives the record with the start key and the newest UpdatedAt. A bad file is skipped.
func (s *Store) ByKey(key string) (Record, bool, error) {
	all, err := s.List()
	if err != nil {
		return Record{}, false, err
	}
	var best Record
	found := false
	for _, r := range all {
		if r.Key != key {
			continue
		}
		if !found || r.UpdatedAt.After(best.UpdatedAt) {
			best, found = r, true
		}
	}
	return best, found, nil
}

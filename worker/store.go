package worker

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/oversire/cli/config"
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

// clone gives a copy that shares no map or pointer with r.
func (r Record) clone() Record {
	r.Cursors = maps.Clone(r.Cursors)
	if r.Escalation != nil {
		esc := *r.Escalation
		r.Escalation = &esc
	}
	return r
}

// Store keeps one JSON file for each worker session in its folder: <dir>/<session>.json.
// It reads the folder one time and keeps the records in memory. Each Save writes the file first.
// The daemon is the only writer, so it sees a hand edit of a file only after a restart.
type Store struct {
	dir string

	mu     sync.Mutex
	loaded bool
	recs   map[string]Record // by file name without .json
	bad    map[string]error  // files that do not decode
}

func NewStore(dir string) *Store { return &Store{dir: dir} }

func (s *Store) path(session string) (string, error) {
	if session == "" || session != filepath.Base(session) || strings.HasPrefix(session, ".") {
		return "", fmt.Errorf("bad session id %q", session)
	}
	return filepath.Join(s.dir, session+".json"), nil
}

// load reads the folder when it did not yet. The caller holds s.mu.
func (s *Store) load() error {
	if s.loaded {
		return nil
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	recs, bad := map[string]Record{}, map[string]error{}
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || e.IsDir() {
			continue
		}
		path, err := s.path(name)
		if err != nil {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			bad[name] = err
			continue
		}
		var r Record
		if err := json.Unmarshal(data, &r); err != nil {
			bad[name] = fmt.Errorf("%s: %w", e.Name(), err)
			continue
		}
		recs[name] = r
	}
	s.recs, s.bad, s.loaded = recs, bad, true
	return nil
}

// Load gives the record of the session. A session with no record gives an empty Record.
func (s *Store) Load(session string) (Record, error) {
	if _, err := s.path(session); err != nil {
		return Record{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return Record{}, err
	}
	if err := s.bad[session]; err != nil {
		return Record{}, err
	}
	return s.recs[session].clone(), nil
}

// Save writes the file, then keeps the record in memory.
func (s *Store) Save(r Record) error {
	path, err := s.path(r.Session)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := config.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	s.recs[r.Session] = r.clone()
	delete(s.bad, r.Session)
	return nil
}

// List gives all records in the order of the file names. A bad file is skipped.
func (s *Store) List() ([]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(s.recs))
	for name, r := range s.recs {
		if r.Session != "" {
			names = append(names, name)
		}
	}
	sort.Slice(names, func(i, j int) bool { return names[i]+".json" < names[j]+".json" })
	var out []Record
	for _, name := range names {
		out = append(out, s.recs[name].clone())
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

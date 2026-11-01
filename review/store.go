package review

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/oversire/cli/wire"
)

// record is the review state of one key and kind on this machine.
type record struct {
	Key      string               `json:"key"`
	Kind     wire.ReviewKind      `json:"kind"`
	Comments []wire.ReviewComment `json:"comments"`
	Rounds   []wire.ReviewRound   `json:"rounds"`
	// Prompts holds the prompt of each round that requests changes, by round number.
	Prompts map[int]string `json:"prompts,omitempty"`
	// Viewed holds the files that the user marked, as repo + "/" + path.
	Viewed  map[string]bool `json:"viewed,omitempty"`
	Note    string          `json:"note,omitempty"`
	ReadyAt *time.Time      `json:"readyAt,omitempty"`
	Plan    *wire.PlanDoc   `json:"plan,omitempty"`
	// Root is the workspace root of a code review, or the folder of a plan.
	Root     string     `json:"root,omitempty"`
	Conflict bool       `json:"conflict,omitempty"`
	MergedAt *time.Time `json:"mergedAt,omitempty"`
	// Approved is true after a plan round that approves. A new plan or a hand-off clears it.
	Approved bool `json:"approved,omitempty"`
	// Agent is the agent of the last known session, for a new session after it ended.
	Agent *agentMemo `json:"agent,omitempty"`
	// Session is the DaemonSession id of the last known session.
	Session   string    `json:"session,omitempty"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// agentMemo is the runtime and the agent of a session.
type agentMemo struct {
	Runtime string           `json:"runtime"`
	Launch  wire.AgentLaunch `json:"launch"`
}

// Store keeps one JSON file for each key and kind in its folder: <dir>/<kind>-<key>.json,
// and the roots that a hand-off named in <dir>/roots.json.
type Store struct{ dir string }

func NewStore(dir string) *Store { return &Store{dir: dir} }

func (s *Store) Save(r *record) error {
	return s.write(string(r.Kind)+"-"+r.Key+".json", r)
}

// Load reads each record file. A bad file is in the error, and the other records load.
func (s *Store) Load() ([]*record, error) {
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*record
	var errs []error
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") || !strings.HasPrefix(name, "code-") && !strings.HasPrefix(name, "plan-") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.dir, name))
		r := &record{}
		if err == nil {
			err = json.Unmarshal(data, r)
		}
		if err == nil && (r.Key == "" || r.Kind == "") {
			err = errors.New("no key or kind")
		}
		if err != nil {
			errs = append(errs, errors.New(name+": "+err.Error()))
			continue
		}
		out = append(out, r)
	}
	return out, errors.Join(errs...)
}

func (s *Store) Roots() ([]string, error) {
	data, err := os.ReadFile(filepath.Join(s.dir, "roots.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var roots []string
	return roots, json.Unmarshal(data, &roots)
}

func (s *Store) SaveRoots(roots []string) error { return s.write("roots.json", roots) }

// write writes v to a temp file, then renames it, so a crash never leaves half a file.
func (s *Store) write(name string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(s.dir, name)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

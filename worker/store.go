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
	Session   string         `json:"session"`
	Key       string         `json:"key,omitempty"`
	Cursors   map[string]int `json:"cursors,omitempty"`
	UpdatedAt time.Time      `json:"updatedAt"`
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

// ByKey gives the record with the start key and the newest UpdatedAt. A bad file is skipped.
func (s *Store) ByKey(key string) (Record, bool, error) {
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, os.ErrNotExist) {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, err
	}
	var best Record
	found := false
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || e.IsDir() {
			continue
		}
		r, err := s.Load(name)
		if err != nil || r.Key != key {
			continue
		}
		if !found || r.UpdatedAt.After(best.UpdatedAt) {
			best, found = r, true
		}
	}
	return best, found, nil
}

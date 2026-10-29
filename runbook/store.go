package runbook

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/oversire/cli/wire"
)

// Store keeps one JSON file for each run in its folder: <dir>/<run>.json.
type Store struct{ dir string }

func NewStore(dir string) *Store { return &Store{dir: dir} }

// Save writes the run to a temp file, then renames it, so a crash never leaves half a file.
func (s *Store) Save(r wire.RunbookRun) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(s.dir, r.ID+".json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Load reads each run file. A bad file is in the error, and the other runs load.
func (s *Store) Load() ([]wire.RunbookRun, error) {
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []wire.RunbookRun
	var errs []error
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		var r wire.RunbookRun
		if err == nil {
			err = json.Unmarshal(data, &r)
		}
		if err == nil && r.ID == "" {
			err = errors.New("no run id")
		}
		if err != nil {
			errs = append(errs, errors.New(e.Name()+": "+err.Error()))
			continue
		}
		out = append(out, r)
	}
	return out, errors.Join(errs...)
}

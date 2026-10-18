package config

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

// State is what `run` writes so that `status` can show it.
type State struct {
	Connected bool       `json:"connected"`
	Since     *time.Time `json:"since,omitempty"`
	Device    string     `json:"device"`
	Hub       string     `json:"hub"`
	LastOrder string     `json:"lastOrder,omitempty"`
	LastError string     `json:"lastError,omitempty"`
	Version   string     `json:"version"`
}

// StateFile keeps State in memory and writes it on each change.
type StateFile struct {
	path string
	mu   sync.Mutex
	s    State
}

func NewStateFile(path string, s State) *StateFile {
	f := &StateFile{path: path, s: s}
	f.write()
	return f
}

func (f *StateFile) Connected(since time.Time) {
	f.update(func(s *State) { s.Connected, s.Since, s.LastError = true, &since, "" })
}

func (f *StateFile) Disconnected(err error) {
	f.update(func(s *State) {
		s.Connected, s.Since = false, nil
		if err != nil {
			s.LastError = err.Error()
		}
	})
}

func (f *StateFile) Order(id string) { f.update(func(s *State) { s.LastOrder = id }) }

func (f *StateFile) update(change func(*State)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(&f.s)
	f.write()
}

func (f *StateFile) write() { _ = WriteJSON(f.path, f.s) }

// LoadState gives a zero State when the file is missing.
func LoadState(path string) (State, error) {
	var s State
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(b, &s)
}

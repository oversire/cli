// Package agentsecret keeps the secret env values of the agents on the home daemon (OVERSIRE-401).
// A value goes to the Keychain on stdin, never in argv. It leaves home only for a daemon of the group, at a launch,
// and the runner keeps it in memory only. No log line, event or error holds a value.
package agentsecret

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/oversire/cli/secret"
	"github.com/oversire/cli/wire"
)

// Service is the Keychain service of the values. The account is <scope>/<agent>/<KEY>.
const Service = "oversire.agent-env"

// Keychain keeps one value for each account in the Keychain (secret.Store), with a 0600 file for each account as the fallback.
// The value is base64 in the item, because secret.Store refuses quotes, backslashes and new lines.
type Keychain struct {
	run  secret.Runner
	goos string
	dir  string
	look func(string) (string, error)
}

// NewKeychain makes the Keychain. dir holds the fallback files. goos "" uses the files only.
func NewKeychain(run secret.Runner, goos, dir string, look func(string) (string, error)) *Keychain {
	return &Keychain{run: run, goos: goos, dir: dir, look: look}
}

func (k *Keychain) item(account string) *secret.Store {
	sum := sha256.Sum256([]byte(account))
	return secret.New(k.run, k.goos, Service, filepath.Join(k.dir, hex.EncodeToString(sum[:16])), k.look)
}

func (k *Keychain) Set(ctx context.Context, account, value string) error {
	if err := os.MkdirAll(k.dir, 0o700); err != nil {
		return err
	}
	return k.item(account).Set(ctx, account, base64.StdEncoding.EncodeToString([]byte(value)))
}

func (k *Keychain) Get(ctx context.Context, account string) (string, error) {
	v, err := k.item(account).Get(ctx, account)
	if err != nil {
		return "", err
	}
	b, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		return "", errors.New("agentsecret: the stored value is not valid")
	}
	return string(b), nil
}

func (k *Keychain) Delete(ctx context.Context, account string) { k.item(account).Delete(ctx, account) }

// Vault keeps one value for each account (*Keychain).
type Vault interface {
	Set(ctx context.Context, account, value string) error
	Get(ctx context.Context, account string) (string, error)
	Delete(ctx context.Context, account string)
}

// Store is the values in the vault plus an index of the keys and their set times. GET reads the index only.
type Store struct {
	vault Vault
	path  string
	clock func() time.Time

	mu   sync.Mutex
	keys []wire.AgentSecretKey
}

// NewStore reads the index at path (<data>/agent-secrets.json) when it exists.
func NewStore(vault Vault, path string, clock func() time.Time) (*Store, error) {
	if clock == nil {
		clock = time.Now
	}
	s := &Store{vault: vault, path: path, clock: clock, keys: []wire.AgentSecretKey{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var l wire.AgentSecretList
	if err := json.Unmarshal(data, &l); err != nil {
		return nil, fmt.Errorf("agentsecret: read %s: %w", path, err)
	}
	if l.Secrets != nil {
		s.keys = l.Secrets
	}
	return s, nil
}

func account(scope, agent, key string) string { return scope + "/" + agent + "/" + key }

// Set keeps the value and gives its index row.
func (s *Store) Set(ctx context.Context, scope, agent, key, value string) (wire.AgentSecretKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.vault.Set(ctx, account(scope, agent, key), value); err != nil {
		return wire.AgentSecretKey{}, fmt.Errorf("The secret store did not keep %s.", key)
	}
	row := wire.AgentSecretKey{Scope: scope, Agent: agent, Key: key, SetAt: s.clock().UTC().Truncate(time.Second)}
	s.keys = slices.DeleteFunc(s.keys, func(k wire.AgentSecretKey) bool { return k.Scope == scope && k.Agent == agent && k.Key == key })
	s.keys = append(s.keys, row)
	return row, s.saveLocked()
}

// Delete removes the value. ok is false when the index had no such key.
func (s *Store) Delete(ctx context.Context, scope, agent, key string) (ok bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.keys)
	s.keys = slices.DeleteFunc(s.keys, func(k wire.AgentSecretKey) bool { return k.Scope == scope && k.Agent == agent && k.Key == key })
	s.vault.Delete(ctx, account(scope, agent, key))
	if len(s.keys) == n {
		return false, nil
	}
	return true, s.saveLocked()
}

// DropAgent removes each value of scope and agent, and gives the removed rows.
func (s *Store) DropAgent(ctx context.Context, scope, agent string) ([]wire.AgentSecretKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var gone []wire.AgentSecretKey
	s.keys = slices.DeleteFunc(s.keys, func(k wire.AgentSecretKey) bool {
		if k.Scope == scope && k.Agent == agent {
			gone = append(gone, k)
			return true
		}
		return false
	})
	for _, k := range gone {
		s.vault.Delete(ctx, account(k.Scope, k.Agent, k.Key))
	}
	if len(gone) == 0 {
		return nil, nil
	}
	return gone, s.saveLocked()
}

// List gives the index, sorted by scope, agent and key.
func (s *Store) List() []wire.AgentSecretKey {
	s.mu.Lock()
	out := slices.Clone(s.keys)
	s.mu.Unlock()
	slices.SortFunc(out, func(a, b wire.AgentSecretKey) int {
		return strings.Compare(account(a.Scope, a.Agent, a.Key), account(b.Scope, b.Agent, b.Key))
	})
	return out
}

// Value gives the value of one secret. ok is false when the index or the vault has none.
func (s *Store) Value(ctx context.Context, scope, agent, key string) (value string, ok bool) {
	s.mu.Lock()
	known := slices.ContainsFunc(s.keys, func(k wire.AgentSecretKey) bool { return k.Scope == scope && k.Agent == agent && k.Key == key })
	s.mu.Unlock()
	if !known {
		return "", false
	}
	v, err := s.vault.Get(ctx, account(scope, agent, key))
	return v, err == nil
}

func (s *Store) saveLocked() error {
	data, err := json.MarshalIndent(wire.AgentSecretList{Secrets: s.keys}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

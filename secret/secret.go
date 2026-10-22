// Package secret stores tokens and keys of the daemon. The token goes to a child process on stdin, never in argv.
//
// macOS: the login Keychain through `security -i`. Linux: the Secret Service through secret-tool.
// Else, or when that fails: a file with mode 0600. Windows uses the file for now (Credential Manager later).
package secret

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Keychain services.
const (
	Daemon = "oversire.daemon" // the hub device token
	Group  = "oversire.group"  // the group secret
	Legacy = "oversire-runner" // the token of oversire-runner
)

// Runner runs one child process.
type Runner interface {
	Run(ctx context.Context, exe string, args []string, stdin []byte) (stdout, stderr []byte, err error)
}

type backend int

const (
	fileOnly backend = iota
	keychain
	secretTool
)

type Store struct {
	run     Runner
	service string
	file    string
	backend backend
}

// New picks the backend for goos. service names the keychain item. file is the fallback path.
// look finds a program on PATH.
func New(run Runner, goos, service, file string, look func(string) (string, error)) *Store {
	s := &Store{run: run, service: service, file: file}
	switch goos {
	case "darwin":
		s.backend = keychain
	case "linux":
		if _, err := look("secret-tool"); err == nil {
			s.backend = secretTool
		}
	}
	return s
}

// Where names the place of the token, for status output.
func (s *Store) Where() string {
	switch s.backend {
	case keychain:
		return "keychain"
	case secretTool:
		return "secret-service"
	}
	return s.file
}

func (s *Store) Set(ctx context.Context, device, token string) error {
	if strings.ContainsAny(token, "\"\\\n\r") {
		return errors.New("secret: the token has bad characters")
	}
	var err error
	switch s.backend {
	case keychain:
		cmd := fmt.Sprintf("add-generic-password -U -s %s -a %s -w \"%s\"\n", s.service, quote(device), token)
		_, stderr, e := s.run.Run(ctx, "/usr/bin/security", []string{"-i"}, []byte(cmd))
		err = commandError("security", stderr, e)
		// `security -i` exits 0 on a failed command, so read the token back.
		if err == nil {
			if got, e := s.Get(ctx, device); e != nil || got != token {
				err = errors.New("secret: the keychain did not keep the token")
			}
		}
	case secretTool:
		_, stderr, e := s.run.Run(ctx, "secret-tool",
			[]string{"store", "--label=OverSire daemon", "service", s.service, "account", device}, []byte(token))
		err = commandError("secret-tool", stderr, e)
	default:
		return s.writeFile(token)
	}
	if err != nil {
		return s.writeFile(token)
	}
	_ = os.Remove(s.file)
	return nil
}

func (s *Store) Get(ctx context.Context, device string) (string, error) {
	var out []byte
	var err error
	switch s.backend {
	case keychain:
		out, _, err = s.run.Run(ctx, "/usr/bin/security", []string{"find-generic-password", "-s", s.service, "-a", device, "-w"}, nil)
	case secretTool:
		out, _, err = s.run.Run(ctx, "secret-tool", []string{"lookup", "service", s.service, "account", device}, nil)
	default:
		err = errors.New("no keychain")
	}
	if t := strings.TrimSpace(string(out)); err == nil && t != "" {
		return t, nil
	}
	b, ferr := os.ReadFile(s.file)
	if ferr != nil {
		return "", errors.New("secret: no token")
	}
	return strings.TrimSpace(string(b)), nil
}

func (s *Store) Delete(ctx context.Context, device string) {
	switch s.backend {
	case keychain:
		_, _, _ = s.run.Run(ctx, "/usr/bin/security", []string{"delete-generic-password", "-s", s.service, "-a", device}, nil)
	case secretTool:
		_, _, _ = s.run.Run(ctx, "secret-tool", []string{"clear", "service", s.service, "account", device}, nil)
	}
	_ = os.Remove(s.file)
}

func (s *Store) writeFile(token string) error {
	if err := os.WriteFile(s.file, []byte(token+"\n"), 0o600); err != nil {
		return err
	}
	return os.Chmod(s.file, 0o600)
}

func quote(v string) string {
	return "\"" + strings.NewReplacer("\\", "\\\\", "\"", "\\\"").Replace(v) + "\""
}

func commandError(name string, stderr []byte, err error) error {
	if err == nil {
		return nil
	}
	if t := strings.TrimSpace(string(stderr)); t != "" {
		return fmt.Errorf("%s: %s", name, t)
	}
	return fmt.Errorf("%s: %w", name, err)
}

// Package orca drives Orca terminals through the Orca CLI. Each call has an argument array and --json.
package orca

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/oversire/runner/runtime"
	"github.com/oversire/runner/wire"
)

const (
	ID       = "orca"
	MacPath  = "/Applications/Orca.app/Contents/Resources/bin/orca"
	localEnv = "local"
)

// Runner runs one child process.
type Runner interface {
	Run(ctx context.Context, exe string, args []string, stdin []byte) (stdout, stderr []byte, err error)
}

// Error is an error answer of the Orca CLI.
type Error struct{ Code, Message string }

func (e *Error) Error() string { return e.Message + " (" + e.Code + ")" }

// FindCLI gives the Orca CLI of the app on macOS, else orca on PATH. ok is false when there is none.
func FindCLI() (path string, ok bool) {
	if st, err := os.Stat(MacPath); err == nil && !st.IsDir() {
		return MacPath, true
	}
	p, err := exec.LookPath("orca")
	return p, err == nil
}

// agentName keeps the agent command safe to type into a shell.
var agentName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

type Orca struct {
	exe       string
	run       Runner
	shellWait time.Duration
}

func New(exe string, run Runner) *Orca {
	return &Orca{exe: exe, run: run, shellWait: 300 * time.Millisecond}
}

func (o *Orca) ID() string { return ID }

// Start opens an agent session in the workspace with the request name, then sends the prompt once.
// A workspace with that name at the repo path is used again. A new workspace gets a fallback shell,
// and the agent command goes into it.
func (o *Orca) Start(ctx context.Context, _ runtime.Order, r wire.LaunchRequest) (wire.SessionRef, error) {
	var none wire.SessionRef
	name := r.WorkspaceName()
	if name == "" {
		return none, errors.New("A launch needs a name or an item.")
	}
	if !agentName.MatchString(r.Agent) {
		return none, fmt.Errorf("The agent name %q is not valid.", r.Agent)
	}
	env := environment(r.Environment)
	command := AgentCommand(r.Agent, name)
	if r.Command != "" {
		if err := checkCommand(r.Agent, r.Command); err != nil {
			return none, err
		}
		command = r.Command
	}

	card, err := o.findWorkspace(ctx, env, r.Repo, name)
	if err != nil {
		return none, err
	}
	shell := ""
	if card == "" {
		if card, err = o.createWorkspace(ctx, env, r); err != nil {
			return none, err
		}
		shell = o.waitShell(ctx, env, card)
	}

	handle := shell
	if shell != "" {
		if _, err := o.call(ctx, env, 20*time.Second, "terminal", "send", "--terminal", shell, "--text", command, "--enter"); err != nil {
			return none, err
		}
	} else {
		var made struct {
			Terminal struct {
				Handle string `json:"handle"`
			} `json:"terminal"`
		}
		if err := o.callInto(ctx, env, 30*time.Second, &made, "terminal", "create", "--worktree", "id:"+card, "--title", name, "--command", command); err != nil {
			return none, err
		}
		if made.Terminal.Handle == "" {
			return none, errors.New("Orca made no terminal.")
		}
		handle = made.Terminal.Handle
	}
	ref := wire.SessionRef{Workspace: wire.WorkspaceRef{Account: wire.AccountID{Provider: ID, ID: env}, ID: card}, ID: handle}
	if err := o.waitIdle(ctx, env, handle, name); err != nil {
		return none, err
	}
	return ref, o.send(ctx, ref, r.Prompt)
}

func (o *Orca) findWorkspace(ctx context.Context, env, repo, name string) (string, error) {
	var ps struct {
		Worktrees []struct {
			ID          string `json:"worktreeId"`
			Path        string `json:"path"`
			DisplayName string `json:"displayName"`
			IsArchived  bool   `json:"isArchived"`
		} `json:"worktrees"`
	}
	if err := o.callInto(ctx, env, 20*time.Second, &ps, "worktree", "ps", "--limit", "500"); err != nil {
		return "", err
	}
	for _, w := range ps.Worktrees {
		if !w.IsArchived && w.Path == repo && strings.EqualFold(w.DisplayName, name) {
			return w.ID, nil
		}
	}
	return "", nil
}

func (o *Orca) createWorkspace(ctx context.Context, env string, r wire.LaunchRequest) (string, error) {
	args := []string{"worktree", "create", "--repo", "path:" + r.Repo, "--name", r.WorkspaceName()}
	if note := r.WorkspaceNote(); note != "" {
		args = append(args, "--comment", note)
	}
	var made struct {
		Worktree struct {
			ID string `json:"id"`
		} `json:"worktree"`
	}
	err := o.callInto(ctx, env, 30*time.Second, &made, args...)
	if e := (*Error)(nil); errors.As(err, &e) && e.Code == "repo_not_found" {
		return "", &Error{Code: e.Code, Message: "Orca has no workspace at " + r.Repo}
	}
	if err != nil {
		return "", err
	}
	if made.Worktree.ID == "" {
		return "", errors.New("Orca made no workspace.")
	}
	return made.Worktree.ID, nil
}

// waitShell gives the fallback shell of a new workspace, or "" when none shows up.
func (o *Orca) waitShell(ctx context.Context, env, card string) string {
	for i := range 10 {
		var list struct {
			Terminals []struct {
				Handle   string `json:"handle"`
				Orphaned bool   `json:"orphaned"`
			} `json:"terminals"`
		}
		if o.callInto(ctx, env, 20*time.Second, &list, "terminal", "list", "--worktree", "id:"+card) == nil {
			for _, t := range list.Terminals {
				if t.Handle != "" && !t.Orphaned {
					return t.Handle
				}
			}
		}
		if i < 9 {
			select {
			case <-ctx.Done():
				return ""
			case <-time.After(o.shellWait):
			}
		}
	}
	return ""
}

// waitIdle waits two times, since a cold start of the agent can take longer than one wait.
func (o *Orca) waitIdle(ctx context.Context, env, handle, name string) error {
	for range 2 {
		var r struct {
			Wait struct {
				Satisfied bool `json:"satisfied"`
			} `json:"wait"`
		}
		err := o.callInto(ctx, env, 60*time.Second, &r, "terminal", "wait", "--terminal", handle, "--for", "tui-idle", "--timeout-ms", "45000")
		if err == nil && r.Wait.Satisfied {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return &Error{Code: "not_started", Message: "The agent did not start in " + name}
}

// AgentCommand starts the agent. Claude gets the workspace name as its session name.
func AgentCommand(agent, name string) string {
	if agent == "claude" {
		return "claude -n '" + strings.ReplaceAll(name, "'", "") + "'"
	}
	return agent
}

// shellOperators may not show in a start command, also not in quotes.
var shellOperators = []string{";", "|", "&", "`", "$(", ">", "<", "\n"}

// checkCommand accepts a start command only if its first word is the agent (or a path to it) and it has no shell operator.
func checkCommand(agent, cmd string) error {
	for _, op := range shellOperators {
		if strings.Contains(cmd, op) {
			if op == "\n" {
				op = "newline"
			}
			return fmt.Errorf("The command has the shell operator %s. The runner does not run it.", op)
		}
	}
	words := strings.Fields(cmd)
	if len(words) == 0 || (words[0] != agent && !strings.HasSuffix(words[0], "/"+agent)) {
		return fmt.Errorf("The command must start with %s.", agent)
	}
	return nil
}

func (o *Orca) Stop(ctx context.Context, s wire.SessionRef) error {
	_, err := o.call(ctx, sessionEnv(s), 20*time.Second, "terminal", "close", "--terminal", s.ID)
	return err
}

// Send types the text into the terminal. Orca has no permission prompts, so a permission answer fails.
func (o *Orca) Send(ctx context.Context, p wire.SendPayload) error {
	if p.Permission != nil {
		return errors.New("The orca runtime has no permission prompts. Answer the agent in its terminal.")
	}
	return o.send(ctx, p.Session, p.Text)
}

func (o *Orca) send(ctx context.Context, s wire.SessionRef, text string) error {
	_, err := o.call(ctx, sessionEnv(s), 30*time.Second, "terminal", "send", "--terminal", s.ID, "--text", text, "--enter", "--wait-submit", "15")
	return err
}

// Read gives the last lines of the screen without blank lines and rules.
func (o *Orca) Read(ctx context.Context, s wire.SessionRef, lines int) ([]string, error) {
	var r struct {
		Terminal struct {
			Tail []string `json:"tail"`
		} `json:"terminal"`
	}
	if err := o.callInto(ctx, sessionEnv(s), 20*time.Second, &r, "terminal", "read", "--terminal", s.ID, "--screen"); err != nil {
		return nil, err
	}
	out := []string{}
	for _, l := range r.Terminal.Tail {
		l = strings.TrimSpace(strings.ReplaceAll(l, " ", " "))
		if l == "" || strings.Trim(l, "─") == "" {
			continue
		}
		out = append(out, l)
	}
	if lines > 0 && len(out) > lines {
		out = out[len(out)-lines:]
	}
	return out, nil
}

func environment(env string) string {
	if env == "" {
		return localEnv
	}
	return env
}

func sessionEnv(s wire.SessionRef) string { return environment(s.Workspace.Account.ID) }

func (o *Orca) callInto(ctx context.Context, env string, timeout time.Duration, v any, args ...string) error {
	raw, err := o.call(ctx, env, timeout, args...)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("orca %s: %w", strings.Join(args[:2], " "), err)
	}
	return nil
}

// call runs the CLI and gives the result object. A non-local environment adds --environment.
func (o *Orca) call(ctx context.Context, env string, timeout time.Duration, args ...string) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	full := append(slices.Clone(args), "--json")
	if env != localEnv {
		full = append(full, "--environment", env)
	}
	stdout, stderr, runErr := o.run.Run(ctx, o.exe, full, nil)
	command := strings.Join(args[:min(2, len(args))], " ")
	var reply struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(stdout, &reply); err != nil {
		text := strings.TrimSpace(string(stderr))
		if text == "" && runErr != nil {
			text = runErr.Error()
		}
		if text == "" {
			text = "no JSON reply"
		}
		return nil, fmt.Errorf("orca %s: %s", command, text)
	}
	if reply.OK {
		if len(reply.Result) == 0 {
			return json.RawMessage("{}"), nil
		}
		return reply.Result, nil
	}
	e := &Error{Code: "error", Message: "orca " + command + " failed"}
	if reply.Error != nil {
		if reply.Error.Code != "" {
			e.Code = reply.Error.Code
		}
		if reply.Error.Message != "" {
			e.Message = reply.Error.Message
		}
	}
	return nil, e
}

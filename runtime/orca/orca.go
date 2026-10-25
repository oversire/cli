// Package orca drives the Orca of this machine through the Orca CLI. Each call has an argument array and --json.
// The daemon talks only to its own Orca: no call has --environment.
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
	"strconv"
	"strings"
	"time"
	"unicode"

	agentwire "github.com/trebi-ai/agent-wire"

	"github.com/oversire/cli/digest"
	"github.com/oversire/cli/runtime"
	"github.com/oversire/cli/wire"
)

const (
	ID      = "orca"
	MacPath = "/Applications/Orca.app/Contents/Resources/bin/orca"
	// account is the account id of the session refs. The app puts the machine name in its place.
	account = "local"
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
	stat      func(string) (os.FileInfo, error)
	shellWait time.Duration
	// digests gives the transcript reader of a harness.
	digests func(harness string) (digest.Reader, error)
	now     func() time.Time

	pairing *Pairing // nil: the views are text only
}

func New(exe string, run Runner) *Orca {
	return &Orca{exe: exe, run: run, stat: os.Stat, shellWait: 300 * time.Millisecond, digests: digest.ForUser, now: time.Now}
}

// WithPairing turns on the live view through a paired device.
func (o *Orca) WithPairing(p *Pairing) *Orca {
	o.pairing = p
	return o
}

func (o *Orca) ID() string { return ID }

// Start is Launch with only the session.
func (o *Orca) Start(ctx context.Context, ord runtime.Order, r wire.LaunchRequest) (wire.SessionRef, error) {
	st, err := o.Launch(ctx, ord, r)
	return st.Session, err
}

// Launch opens an agent session in the workspace with the request name, then sends the prompt once.
// A workspace with that name at the repo path is used again. A new workspace gets a fallback shell,
// and the agent command goes into it. With InPlace, a new terminal opens in the folder Repo.
func (o *Orca) Launch(ctx context.Context, _ runtime.Order, r wire.LaunchRequest) (runtime.Started, error) {
	var none runtime.Started
	name := r.WorkspaceName()
	if r.InPlace {
		name = r.Title
	}
	if name == "" {
		return none, errors.New("A launch needs a name or an item.")
	}
	if !agentName.MatchString(r.Agent) {
		return none, fmt.Errorf("The agent name %q is not valid.", r.Agent)
	}
	title := r.Title
	if title == "" {
		title = name
	}
	command := AgentCommand(r.Agent, name)
	if r.Command != "" {
		if err := checkCommand(r.Agent, r.Command); err != nil {
			return none, err
		}
		command = r.Command
	}
	command, err := withModel(r.Agent, command, r.Model, r.Effort)
	if err != nil {
		return none, err
	}

	var t terminal
	if r.InPlace {
		if t, err = o.createTerminal(ctx, "path:"+r.Repo, title, command); err != nil {
			return none, err
		}
	} else if t, err = o.workspaceTerminal(ctx, r, name, title, command); err != nil {
		return none, err
	}
	ref := wire.SessionRef{Workspace: wire.WorkspaceRef{Account: wire.AccountID{Provider: ID, ID: account}, ID: t.Worktree}, ID: t.Handle}
	if err := o.waitIdle(ctx, t.Handle, name); err != nil {
		return none, err
	}
	started, err := o.send(ctx, ref, r.Prompt)
	if err != nil {
		return none, err
	}
	return runtime.Started{Session: ref, PaneKey: t.paneKey(), TurnStarted: started}, nil
}

// terminal is one row of `terminal list`, or the terminal of `terminal create`.
type terminal struct {
	Handle   string `json:"handle"`
	Worktree string `json:"worktreeId"`
	Title    string `json:"title"`
	TabID    string `json:"tabId"`
	LeafID   string `json:"leafId"`
	PaneKey  string `json:"paneKey"`
	Recorded string `json:"recordedPaneKey"` // terminal list: the tab and leaf ids can be pty ids
	Orphaned bool   `json:"orphaned"`
}

// paneKey is <tabId>:<leafId>, the id of the session in the session book.
func (t terminal) paneKey() string {
	if t.PaneKey != "" {
		return t.PaneKey
	}
	if t.Recorded != "" {
		return t.Recorded
	}
	if t.TabID == "" || t.LeafID == "" {
		return ""
	}
	return t.TabID + ":" + t.LeafID
}

// workspaceTerminal finds or makes the workspace and starts the agent command in it.
func (o *Orca) workspaceTerminal(ctx context.Context, r wire.LaunchRequest, name, title, command string) (terminal, error) {
	card, err := o.findWorkspace(ctx, r.Repo, name)
	if err != nil {
		return terminal{}, err
	}
	shell, ok := terminal{}, false
	if card == "" {
		if card, err = o.createWorkspace(ctx, r); err != nil {
			return terminal{}, err
		}
		shell, ok = o.waitShell(ctx, card)
	}
	t := shell
	if ok {
		if _, err := o.call(ctx, 20*time.Second, "terminal", "send", "--terminal", shell.Handle, "--text", command, "--enter"); err != nil {
			return terminal{}, err
		}
	} else if t, err = o.createTerminal(ctx, "id:"+card, title, command); err != nil {
		return terminal{}, err
	}
	t.Worktree = card
	return t, nil
}

func (o *Orca) createTerminal(ctx context.Context, worktree, title, command string) (terminal, error) {
	var made struct {
		Terminal terminal `json:"terminal"`
	}
	if err := o.callInto(ctx, 30*time.Second, &made, "terminal", "create", "--worktree", worktree, "--title", title, "--command", command); err != nil {
		return terminal{}, err
	}
	if made.Terminal.Handle == "" {
		return terminal{}, errors.New("Orca made no terminal.")
	}
	if made.Terminal.Worktree == "" {
		made.Terminal.Worktree = strings.TrimPrefix(worktree, "id:")
	}
	return made.Terminal, nil
}

func (o *Orca) findWorkspace(ctx context.Context, repo, name string) (string, error) {
	var ps struct {
		Worktrees []struct {
			ID          string `json:"worktreeId"`
			Path        string `json:"path"`
			DisplayName string `json:"displayName"`
			IsArchived  bool   `json:"isArchived"`
		} `json:"worktrees"`
	}
	if err := o.callInto(ctx, 20*time.Second, &ps, "worktree", "ps", "--limit", "500"); err != nil {
		return "", err
	}
	for _, w := range ps.Worktrees {
		if !w.IsArchived && w.Path == repo && strings.EqualFold(w.DisplayName, name) {
			return w.ID, nil
		}
	}
	return "", nil
}

func (o *Orca) createWorkspace(ctx context.Context, r wire.LaunchRequest) (string, error) {
	args := []string{"worktree", "create", "--repo", "path:" + r.Repo, "--name", r.WorkspaceName()}
	if note := r.WorkspaceNote(); note != "" {
		args = append(args, "--comment", note)
	}
	if r.BaseBranch != "" {
		args = append(args, "--base-branch", r.BaseBranch)
	}
	var made struct {
		Worktree struct {
			ID string `json:"id"`
		} `json:"worktree"`
	}
	err := o.callInto(ctx, 30*time.Second, &made, args...)
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

// waitShell gives the fallback shell of a new workspace. ok is false when none shows up.
func (o *Orca) waitShell(ctx context.Context, card string) (terminal, bool) {
	for i := range 10 {
		var list struct {
			Terminals []terminal `json:"terminals"`
		}
		if o.callInto(ctx, 20*time.Second, &list, "terminal", "list", "--worktree", "id:"+card) == nil {
			for _, t := range list.Terminals {
				if t.Handle != "" && !t.Orphaned {
					return t, true
				}
			}
		}
		if i < 9 {
			select {
			case <-ctx.Done():
				return terminal{}, false
			case <-time.After(o.shellWait):
			}
		}
	}
	return terminal{}, false
}

// waitIdle waits two times, since a cold start of the agent can take longer than one wait.
func (o *Orca) waitIdle(ctx context.Context, handle, name string) error {
	for range 2 {
		var r struct {
			Wait struct {
				Satisfied bool `json:"satisfied"`
			} `json:"wait"`
		}
		err := o.callInto(ctx, 60*time.Second, &r, "terminal", "wait", "--terminal", handle, "--for", "tui-idle", "--timeout-ms", "45000")
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

// modelValue keeps a model or effort value safe to type into a shell, in single quotes when needed.
var (
	modelValue = regexp.MustCompile(`^[A-Za-z0-9._/:@+=,\[\]-]+$`)
	plainValue = regexp.MustCompile(`^[A-Za-z0-9._/:@+=,-]+$`)
)

// Flags that set the model or the effort on some CLI. A command with one of them keeps its own value.
var (
	modelFlags  = []string{"--model", "-m"}
	effortFlags = []string{"--effort", "--thinking", "--reasoning-effort", "model_reasoning_effort"}
)

// withModel adds the agent-wire model and effort flags of the base CLI to the command.
// A flag that the command has already stays as it is: the command wins.
func withModel(agent, command, model, effort string) (string, error) {
	words := strings.Fields(command)
	var add []string
	for _, v := range []struct {
		value, label string
		flags        []string
		args         []string
	}{
		{model, "model", modelFlags, agentwire.ModelArgs(agentwire.Harness(agent), model, "")},
		{effort, "effort", effortFlags, agentwire.ModelArgs(agentwire.Harness(agent), "", effort)},
	} {
		if v.value == "" || len(v.args) == 0 || hasAnyFlag(words, v.flags) {
			continue
		}
		if !modelValue.MatchString(v.value) {
			return "", fmt.Errorf("The %s %q is not valid.", v.label, v.value)
		}
		for _, a := range v.args {
			if !plainValue.MatchString(a) {
				a = "'" + a + "'"
			}
			add = append(add, a)
		}
	}
	if len(add) == 0 {
		return command, nil
	}
	return command + " " + strings.Join(add, " "), nil
}

// hasAnyFlag is true when a word is one of the flags, sets it with =, or names it in -c key=value.
func hasAnyFlag(words, flags []string) bool {
	for _, w := range words {
		for _, f := range flags {
			if w == f || strings.HasPrefix(w, f+"=") || strings.HasPrefix(strings.Trim(w, `'"`), f+"=") {
				return true
			}
		}
	}
	return false
}

func (o *Orca) Stop(ctx context.Context, s wire.SessionRef) error {
	_, err := o.call(ctx, 20*time.Second, "terminal", "close", "--terminal", s.ID)
	return err
}

func (o *Orca) Send(ctx context.Context, p wire.SendPayload) error {
	_, err := o.Submit(ctx, p)
	return err
}

// Submit types the text into the terminal. Orca has no permission prompts, so a permission answer fails.
func (o *Orca) Submit(ctx context.Context, p wire.SendPayload) (bool, error) {
	if p.Permission != nil {
		return false, errors.New("The orca runtime has no permission prompts. Answer the agent in its terminal.")
	}
	if p.Answer != nil {
		return false, errors.New("The orca runtime has no runner questions. Answer the agent in Orca.")
	}
	return o.send(ctx, p.Session, p.Text)
}

// send is true when the agent started a turn on the text.
func (o *Orca) send(ctx context.Context, s wire.SessionRef, text string) (bool, error) {
	var r struct {
		Send struct {
			Prompt struct {
				Stages []string `json:"stages"`
			} `json:"prompt"`
		} `json:"send"`
	}
	if err := o.callInto(ctx, 30*time.Second, &r, "terminal", "send", "--terminal", s.ID, "--text", text, "--enter", "--wait-submit", "15"); err != nil {
		return false, err
	}
	return slices.Contains(r.Send.Prompt.Stages, "turn_started"), nil
}

// Read gives the last lines of the screen without blank lines and rules.
func (o *Orca) Read(ctx context.Context, s wire.SessionRef, lines int) ([]string, error) {
	var r struct {
		Terminal struct {
			Tail []string `json:"tail"`
		} `json:"terminal"`
	}
	if err := o.callInto(ctx, 20*time.Second, &r, "terminal", "read", "--terminal", s.ID, "--screen"); err != nil {
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

// ReadScrollback gives the last lines of the output, with blank lines.
func (o *Orca) ReadScrollback(ctx context.Context, s wire.SessionRef, lines int) ([]string, error) {
	var r struct {
		Terminal struct {
			Tail []string `json:"tail"`
		} `json:"terminal"`
	}
	if err := o.callInto(ctx, 20*time.Second, &r, "terminal", "read", "--terminal", s.ID, "--limit", strconv.Itoa(max(lines, 1))); err != nil {
		return nil, err
	}
	out := make([]string, len(r.Terminal.Tail))
	for i, l := range r.Terminal.Tail {
		out[i] = strings.ReplaceAll(l, "\u00a0", " ")
	}
	return out, nil
}

// History finds the transcript of the agent in the terminal through orca search, then reads it.
// The session is the newest one of the agent at the workspace path with the terminal title as its name.
// Orca gives the transcript path only for a session on this machine.
func (o *Orca) History(ctx context.Context, s wire.SessionRef, b wire.HistoryBudget) (wire.SessionDigest, error) {
	var none wire.SessionDigest
	var shown struct {
		Terminal struct {
			Title         string `json:"title"`
			WorktreePath  string `json:"worktreePath"`
			AgentIdentity string `json:"agentIdentity"`
		} `json:"terminal"`
	}
	if err := o.callInto(ctx, 20*time.Second, &shown, "terminal", "show", "--terminal", s.ID); err != nil {
		return none, err
	}
	t := shown.Terminal
	if t.AgentIdentity == "" {
		return none, fmt.Errorf("Orca knows no agent in the terminal %s.", s.ID)
	}
	reader, err := o.digests(t.AgentIdentity)
	if err != nil {
		return none, err
	}
	// The agent puts a status glyph in front of the session name.
	name := strings.TrimLeftFunc(t.Title, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	if name == "" || t.WorktreePath == "" {
		return none, fmt.Errorf("Orca gives no session name or path for the terminal %s.", s.ID)
	}
	var found struct {
		Hits []struct {
			Title  string `json:"title"`
			Source struct {
				FilePath string `json:"filePath"`
			} `json:"source"`
		} `json:"hits"`
	}
	if err := o.callInto(ctx, 30*time.Second, &found, "search", "--query", name, "--agent", t.AgentIdentity,
		"--path", t.WorktreePath, "--sort", "newest", "--limit", "100"); err != nil {
		return none, err
	}
	for _, h := range found.Hits {
		if !strings.EqualFold(strings.TrimSpace(h.Title), name) {
			continue
		}
		if h.Source.FilePath == "" {
			return none, fmt.Errorf("Orca gives no transcript path for the session %q.", name)
		}
		return reader.Digest(h.Source.FilePath, &b, wire.DigestFromRunner)
	}
	return none, fmt.Errorf("Orca found no %s session %q at %s.", t.AgentIdentity, name, t.WorktreePath)
}

func (o *Orca) callInto(ctx context.Context, timeout time.Duration, v any, args ...string) error {
	raw, err := o.call(ctx, timeout, args...)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("orca %s: %w", strings.Join(args[:2], " "), err)
	}
	return nil
}

// call runs the CLI and gives the result object.
func (o *Orca) call(ctx context.Context, timeout time.Duration, args ...string) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	full := append(slices.Clone(args), "--json")
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

// Command oversire is the OverSire CLI. Its daemon serves the live state of this machine to OverSire clients,
// and does work orders from the hub.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	goruntime "runtime"
	"syscall"

	"github.com/oversire/cli/config"
	"github.com/oversire/cli/group"
	"github.com/oversire/cli/migrate"
	"github.com/oversire/cli/proc"
	"github.com/oversire/cli/secret"
	"github.com/oversire/cli/service"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `oversire is the OverSire CLI.

Usage:
  oversire daemon run                                  run the daemon (the service runs this)
  oversire daemon install [--keep-awake]               start the daemon as a service at login
  oversire daemon uninstall                            remove the service
  oversire daemon status [--json]                      show the state (short form: oversire status)
  oversire hub pair <code> [--hub URL] [--name NAME]   pair with the cloud hub as a runner
  oversire hub pair - [--hub URL] [--name NAME]        read the code from stdin
  oversire hooks install [--harness claude,codex]      add or update the OverSire agent hooks
  oversire hooks remove [--harness claude,codex]       remove only the OverSire agent hooks
  oversire hooks status [--json]                       show the hook state of each harness
  oversire hooks ci-redirect [on|off]                  show or set the refusal of gh and glab CI polls
  oversire group show [--json]                         show the machines and apps of the group
  oversire group invite | requests | approve <id> | deny <id>
  oversire group request-code | admit <machine> <code>
  oversire group remove <machine|deviceId> | home <machine> | rotate
  oversire join <host> <code>|-                        join the group of the member at host
  oversire orca pair | unpair | status [--json]        pair the live Orca view (the link goes on stdin)
  oversire digest --harness claude --session <id or file> [--markers]
                  [--source S] [--earlier N] [--full N] [--max-chars N]
                                                       print the history digest of a session
  oversire step done --run <id> --step <step> --verdict pass|gaps|done|skipped|blocked [--note <text>]
                                                       end a runbook step (the agent of the step runs it)
  oversire epic list [--project P]                     list the epics (the app on this Mac runs each epic command)
  oversire epic show <epic> --project P                show an epic: progress, forecast and numbered steps
  oversire epic plan [<epic>] --project P              show the plan of the open epics, or of one epic
  oversire epic create --project P --title T [--goal G] [--start D] [--target D] [--lead ID]
  oversire epic update <epic> --project P [--title T] [--goal G] [--state S] [--start D] [--target D] [--lead ID]
                         [--health on_track|at_risk|off_track [--note-file F]]
  oversire epic close <epic> --project P               set an epic to done
  oversire epic add <epic> KEY... [--project P]        put items in an epic
  oversire epic remove KEY... [--project P]            take items out of their epic
  oversire project north-star --project P [--set TEXT] show or set the north star of a project
  oversire project statuses|labels|people [P] [--project P]
                                                       list the statuses, labels or tracker members of a project
  oversire project labels [P] --add NAME [--color #RRGGBB]  make a label in the tracker project
  oversire project refresh [P] [--project P]           load the project from the tracker again
  oversire item list [--project P] [--group G[,G]] [--mine] [--label L] [--text T] [--limit N]
                                                       list the loaded items (the app on this Mac runs each item command)
  oversire item search <text> [--project P] [--limit N]  search the items of the trackers
  oversire item show <KEY> [--project P]               print an item and its comments as Markdown
  oversire item create --project P --title T [--body-file F|-] [--status S] [--priority X] [--parent KEY] [--label L]... [--epic E]
                                                       make an item and print its key
  oversire item edit <KEY> [--title T] [--body-file F|-]  change the title or the description
  oversire item status <KEY> <status> [--force] [--sub-items]  set the status: id, name, todo, progress, review, blocked or done
  oversire item start <KEY>                            set the item in progress and assign it to you
  oversire item done <KEY> [--force] [--sub-items]     set the item to done
  oversire item reopen <KEY>                           move a done item back to in progress
  oversire item assign <KEY> --me | PERSON... | --none  add you, set the assignees, or remove all
  oversire item priority <KEY> <name>                  set the priority
  oversire item label <KEY> --add|--remove|--set NAME... | --none  add, remove or set the labels, by name
  oversire item areas <KEY> AREA... | --none           set the code areas (at most 6)
  oversire item comment <KEY> --body-file F|-          add a Markdown comment
  oversire item relate <KEY> --blocks|--blocked-by|--relates-to|--duplicate-of OTHER [--remove]
                                                       add or remove a relation
  oversire item parent <KEY> <PARENT|none>             set or remove the parent
  oversire item link <KEY> <URL> [--title T]           add a web link
  oversire item attach <KEY> <FILE> [--name N]         upload a file (at most 10 MB)
  oversire intake create --project P --title T [--body-file F|-] [--priority X]
                                                       make an intake item and print its key
  oversire intake list [--project P] [--status pending|snoozed|all] [--detail] [--limit N]
                                                       list the intake items
  oversire intake show <KEY> [--project P]             show one intake item with its body
  oversire intake accept|decline <KEY> [--comment-file F|-]
                                                       accept or decline an intake item
  oversire intake snooze <KEY> --until YYYY-MM-DD | --days N [--comment-file F|-]
                                                       snooze an intake item
  oversire intake duplicate <KEY> --of KEEP [--comment-file F|-]
                                                       close an intake item as a duplicate
  oversire intake edit <KEY> [--title T] [--body-file F|-] [--priority X] [--parent KEY|none] [--blocks|--blocked-by|--relates-to|--duplicate-of OTHER]
                                                       change an intake item and keep it in the intake
  oversire people list [--project P] [--json]          list the people of the trackers, code hosts and machines
  oversire activity list [--project P] [--unread] [--limit N] [--json]
                                                       list the tracker activity, newest first
  oversire app run <action> ['<json args>']            run an action of the allowed list of the app on this Mac
                                                       epic, project, item, intake, people, activity, app and vendor take --json, --harness H and --session ID
  oversire vendor ls [--json]                          list the vendor CLIs that the app can run, with the version and the state
  oversire vendor run [--project P | --account A] -- <command> [flags]
                                                       run the vendor CLI of the account with the token of the app
  oversire vendor skill <vendor>                       print how to use the vendor CLI through OverSire
  oversire vendor install <vendor>                     install the checked vendor CLI in ~/.config/oversire/vendors (plane, bigtimer)
  oversire vendor remove <vendor>                      delete the OverSire copy of the vendor CLI
  oversire review ready [--key K] [--kind code|plan] [--note <text>] [--plan <file.md>]
                                                       hand off a group or a plan for review (the agent runs it)
  oversire review reply [--key K] <comment id> "<text>"  answer a review comment (the agent runs it)
  oversire review ls [--kind code|plan] [--json]       show the reviews of this machine
  oversire worker start <KEY> --harness H [--model M] [--dir D] [--plan FILE] [--session ID] [--budget-time 3h]
                                                       start the worker session of an item and oversee it (the overseer runs it)
  oversire worker send <id|KEY> "<text>"              give a text to the worker
  oversire worker answer <id|KEY> --permission P --allow|--deny | --question Q "<text>"
                                                       answer the open permission or question of the worker
  oversire worker interrupt <id|KEY>                  stop the current turn of the worker
  oversire worker escalate <id|KEY> ["<question>"]    ask the user; with no question, show the open ask of the worker
  oversire worker stop <id|KEY>                       end the worker session
  oversire worker done --verdict done|blocked [--note <text>]
                                                       report the end of the work (the worker runs it, in its group)
  oversire worker read <id|KEY> [--reader NAME] [--limit N] [--json]
                                                       show the new turns of a worker session since the last read
  oversire worker wait <id|KEY> [--reader NAME] [--timeout 8m]
                                                       block until the worker needs an action, then show the new turns (exit 3: timeout)
  oversire worker brief <id|KEY>                       show the plan, decisions, last report, failed tools and diff of a worker
  oversire visor ls | session | workdir | project | harness | status
                                                       show and control the agent sessions of the daemon
  oversire ticket skill [new|epic|plan|work|delegate|done|groom|sweep|spec] [--install|--refresh] [--harness claude,codex]
                                                       print the body of the oversire agent skill, or install it
  oversire ticket new|work <KEY> [--session ID]        bind this session to a ticket (the agent runs it)
  oversire ticket plan <KEY>... [--session ID]         bind this session to one ticket or a batch (the agent runs it)
  oversire ticket groom <PROJECT> [--session ID]       check the Groom key of the project and list the busy items
  oversire ticket applied groom|sweep --file F [--project P]
                                                       write the event rows of the applied changes (the agent runs it)
  oversire ticket sweep [KEY...] [--project P] [--root DIR] [--stale DAYS] [--json]
                                                       the facts of the started tickets: sessions, review, worktree group, base commits
  oversire ticket stop <SESSION> [--session ID]        end a daemon session of a finished ticket
  oversire ticket done [KEY] [--session ID] [--out DIR]  pack the session transcript for the ticket
  oversire ci status [REF] [--repo R] [--fresh] [--json]
                                                       show the CI state of a commit, branch or PR (default: HEAD)
  oversire ci ls [--branch B] [--sha S] [--pr N] [--workflow W] [--state S] [--limit N] [--json]
                                                       list the CI runs of the repo
  oversire ci show RUN [--jobs] [--attempt N] [--fresh] [--json]  show a run, with its jobs and failures
  oversire ci why RUN [--json]                         show why a run failed: job, step, messages and log tail
  oversire ci explain RUN [--attempt N] [--job J] [--force] [--json]  explain a failed run: cause, fix, confidence (uses the AI engine of the app)
  oversire ci logs RUN|JOB [--failed] [--lines N] [--fresh] [--json]  print the tail of a job log
  oversire ci workflows [--fresh] [--json]             list the workflows of the repo
  oversire ci wait [REF] [--timeout 30m] [--fail-fast] [--settle 60s] [--workflow W]... [--log-lines 40] [--json|--events]
                                                       block until the CI ends (exit 0 pass, 1 fail, 3 no app, 4 timeout, 5 no runs)
  oversire ci rerun RUN [--failed | --job JOB] [--wait]  run a CI run, its failed jobs or one job again (the app does it)
  oversire ci cancel RUN                               cancel a CI run (the app does it)
  oversire ci dispatch WORKFLOW [--ref REF] [-f key=value]... [--wait]
                                                       start a workflow on a ref (default: the branch with an upstream)
  oversire ci approve RUN [--env NAME]... [--comment TEXT]  approve the deployments that wait on a run
                  all ci commands take --repo owner/name; the default is the origin of the git folder
  oversire update                                      install the latest release now
  oversire version                                     show the version
`

// renamed maps the commands of oversire-runner to the new commands.
var renamed = map[string]string{
	"run":       "oversire daemon run",
	"install":   "oversire daemon install",
	"uninstall": "oversire daemon uninstall",
	"pair":      "oversire hub pair",
}

// exitError ends the program with a code.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

// errSilent ends the program without a message. The command printed its own.
var errSilent = errors.New("")

type cli struct {
	paths   config.Paths
	in      io.Reader
	out     io.Writer
	errOut  io.Writer // nil is os.Stderr
	log     *slog.Logger
	level   *slog.LevelVar
	cmds    *proc.Logged // the runner of the daemon commands; it writes a line for each one
	secrets *secret.Store
	service *service.Manager
	migrate func(context.Context) (migrate.Report, error)
	exe     string
	home    string
	wd      string                                                                // the working folder of the review command; empty is the folder of the process
	git     func(ctx context.Context, dir string, args ...string) (string, error) // runs git; nil is the real git
}

func (c *cli) stderr() io.Writer {
	if c.errOut == nil {
		return os.Stderr
	}
	return c.errOut
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if os.Args[1] == "hook" {
		// The hook of an agent CLI: it must be fast and always exit 0.
		_ = hookCLI().hook(context.Background(), os.Args[2:])
		os.Exit(0)
	}
	if os.Args[1] == "visor" {
		// The holders and the hooks of the visor sessions run this. It makes no config folder.
		exit(visorCLI(os.Args[2:]))
	}
	if os.Args[1] == "ticket" {
		// The skill runs it each time it loads, so it makes no config folder.
		exit(ticketCLI().ticket(os.Args[2:]))
	}
	if os.Args[1] == "digest" {
		// digest also runs on hosts without a daemon setup, so it makes no config folder.
		exit(digestCLI().digest(os.Args[2:]))
	}
	c, err := newCLI()
	if err != nil {
		fmt.Fprintln(os.Stderr, "oversire:", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	exit(c.dispatch(ctx, os.Args[1:]))
}

func (c *cli) dispatch(ctx context.Context, args []string) error {
	cmd, rest := args[0], args[1:]
	sub := ""
	if len(rest) > 0 {
		sub = rest[0]
	}
	switch {
	case cmd == "daemon" && sub == "run":
		return c.run(ctx)
	case cmd == "daemon" && sub == "install":
		return c.install(ctx, rest[1:])
	case cmd == "daemon" && sub == "uninstall":
		return c.uninstall(ctx)
	case cmd == "daemon" && sub == "status":
		return c.status(ctx, rest[1:])
	case cmd == "status":
		return c.status(ctx, rest)
	case cmd == "hooks":
		return c.hooks(ctx, rest)
	case cmd == "group":
		return c.group(ctx, rest)
	case cmd == "join":
		return c.join(ctx, rest)
	case cmd == "hub" && sub == "pair":
		return c.pair(ctx, rest[1:])
	case cmd == "orca":
		return c.orca(ctx, rest)
	case cmd == "step":
		return c.step(ctx, rest)
	case cmd == "epic":
		return c.epic(ctx, rest)
	case cmd == "project":
		return c.project(ctx, rest)
	case cmd == "item":
		return c.item(ctx, rest)
	case cmd == "intake":
		return c.intake(ctx, rest)
	case cmd == "people":
		return c.people(ctx, rest)
	case cmd == "activity":
		return c.activity(ctx, rest)
	case cmd == "app":
		return c.app(ctx, rest)
	case cmd == "vendor":
		return c.vendor(ctx, rest)
	case cmd == "review":
		return c.review(ctx, rest)
	case cmd == "worker":
		return c.worker(ctx, rest)
	case cmd == "ci":
		return c.ci(ctx, rest)
	case cmd == "update":
		return c.update(ctx)
	case cmd == "version" || cmd == "--version" || cmd == "-v":
		fmt.Fprintln(c.out, version)
		return nil
	case cmd == "help" || cmd == "--help" || cmd == "-h":
		fmt.Fprint(c.out, usage)
		return nil
	case renamed[cmd] != "":
		return &exitError{2, fmt.Errorf("%q is now %q.", cmd, renamed[cmd])}
	}
	return &exitError{2, fmt.Errorf("unknown command %q\n\n%s", cmd, usage)}
}

func exit(err error) {
	if err == nil {
		os.Exit(0)
	}
	if !errors.Is(err, errSilent) {
		fmt.Fprintln(os.Stderr, "oversire:", err)
	}
	var e *exitError
	if errors.As(err, &e) {
		os.Exit(e.code)
	}
	os.Exit(1)
}

func digestCLI() *cli {
	home, _ := os.UserHomeDir()
	return &cli{out: os.Stdout, home: home}
}

func newCLI() (*cli, error) {
	dir, err := config.Dir()
	if err != nil {
		return nil, err
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	paths := config.Paths{Dir: dir}
	run := proc.Local{}
	level := new(slog.LevelVar)
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	c := &cli{
		paths:   paths,
		in:      os.Stdin,
		out:     os.Stdout,
		log:     log,
		level:   level,
		cmds:    proc.NewLogged(log),
		secrets: secret.New(run, goruntime.GOOS, secret.Daemon, paths.Token(), exec.LookPath),
		service: service.NewManager(goruntime.GOOS, home, os.Getuid(), exe, run),
		exe:     exe,
		home:    home,
	}
	if config.Home() != "" {
		// A home override runs beside the real install. It must not change the machine service.
		return c, os.MkdirAll(dir, 0o700)
	}
	c.migrate = func(ctx context.Context) (migrate.Report, error) {
		layout, err := migrate.Layouts()
		if err != nil {
			return migrate.Report{}, err
		}
		legacyDir, _ := config.LegacyDir()
		old := secret.New(run, goruntime.GOOS, secret.Legacy, filepath.Join(legacyDir, "token"), exec.LookPath)
		m := migrate.NewMigrator(layout, goruntime.GOOS, c.service, old, c.secrets, run, log.Info)
		r, err := m.Run(ctx)
		if err == nil {
			err = os.MkdirAll(dir, 0o700)
		}
		return r, err
	}
	return c, nil
}

// setup migrates an oversire-runner install, then loads the config. A new install gets the role serve.
func (c *cli) setup(ctx context.Context) (config.Config, error) {
	if c.migrate != nil {
		r, err := c.migrate(ctx)
		if err != nil {
			return config.Config{}, err
		}
		for _, n := range r.Notes {
			c.log.Warn(n)
		}
	}
	cfg, ok, err := c.paths.LoadConfig()
	if err != nil {
		return cfg, err
	}
	if !ok {
		cfg.Roles = []string{config.RoleServe}
		if err := c.paths.SaveConfig(cfg); err != nil {
			return cfg, err
		}
	}
	// A fixed name: the host name of a Mac changes with the network, and a new name starts a new group (OVERSIRE-413).
	if cfg.Name == "" {
		cfg.Name = group.SelfName(filepath.Join(c.paths.Dir, "group.json"))
		if cfg.Name == "" {
			cfg.Name = config.ShortHost()
		}
		if err := c.paths.SaveConfig(cfg); err != nil {
			return cfg, err
		}
		c.log.Info("machine name set", "name", cfg.Name)
	}
	if c.level != nil {
		l, err := cfg.LogLevel(os.Getenv("OVERSIRE_LOG_LEVEL"))
		if err != nil {
			c.log.Warn("log level", "err", err)
		}
		c.level.Set(l)
	}
	return cfg, nil
}

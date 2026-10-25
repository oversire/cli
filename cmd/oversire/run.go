package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strconv"
	"sync"
	"time"

	agentwire "github.com/trebi-ai/agent-wire"

	"github.com/oversire/cli/allow"
	"github.com/oversire/cli/config"
	"github.com/oversire/cli/hub"
	"github.com/oversire/cli/orders"
	"github.com/oversire/cli/proc"
	"github.com/oversire/cli/runtime"
	"github.com/oversire/cli/runtime/orca"
	"github.com/oversire/cli/runtime/oversire"
	"github.com/oversire/cli/update"
	"github.com/oversire/cli/wire"
)

// stopWait is the wait after a refused token or hello, so that the service does not retry fast.
const stopWait = time.Hour

// registry has ov, and orca when its CLI is on this machine.
func (c *cli) registry(ov runtime.Runtime) *runtime.Registry {
	rts := []runtime.Runtime{ov}
	if exe, ok := orca.FindCLI(); ok {
		rts = append(rts, orca.New(exe, c.cmds))
	}
	return runtime.NewRegistry(rts...)
}

func (c *cli) run(ctx context.Context) error {
	if goruntime.GOOS == "darwin" {
		if p := proc.LoginPath(ctx, os.Getenv("SHELL")); p != "" {
			os.Setenv("PATH", proc.MergePath(p, os.Getenv("PATH")))
		} else {
			c.log.Warn("no PATH from the login shell; agent CLIs out of the PATH of launchd are not found")
		}
	}
	cfg, err := c.setup(ctx)
	if err != nil {
		return err
	}
	if cfg.KeepAwake && goruntime.GOOS == "darwin" {
		if err := exec.Command("/usr/bin/caffeinate", "-s", "-w", strconv.Itoa(os.Getpid())).Start(); err != nil {
			c.log.Warn("caffeinate failed", "err", err)
		}
	}
	data, err := cfg.Data()
	if err != nil {
		return err
	}
	agents := agentwire.New(agentwire.Options{ClientName: "oversire", ClientVersion: version, LedgerPath: filepath.Join(data, "agents.json"), Logger: c.log})
	_ = agents.Reconcile()
	var client *hub.Client
	reps := &reports{}
	ov := oversire.New(oversire.Options{Runner: cfg.Device, DataDir: data, Slots: cfg.Sessions()}, agents, c.cmds, reps)
	defer func() {
		ctx, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()
		ov.Close(ctx)
		agents.Close(ctx)
	}()
	reg := c.registry(ov)
	c.log.Info("daemon start", "version", version, "roles", cfg.Roles, "hub", cfg.Hub, "device", cfg.Device, "runtimes", reg.IDs(), "data", data)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	updates := update.NewLoop(update.NewUpdater(&http.Client{Timeout: 5 * time.Minute}, proc.Local{}, update.LatestURL, version, goruntime.GOOS, goruntime.GOARCH, c.exe), time.Now)
	updates.Log = c.log
	go updates.Run(ctx)

	var tasks []func(context.Context) error
	// One allow list, journal and order runner for the hub path and the direct path.
	allowList := allow.File{Path: c.paths.Allow()}
	journal := hub.OpenJournal(c.paths.Journal())
	runner := orders.NewRunner(reg, allowList, journal, time.Now)
	if cfg.Has(config.RoleRunner) && cfg.Paired() {
		token, err := c.secrets.Get(ctx, cfg.Device)
		if err != nil {
			return &exitError{2, err}
		}
		state := config.NewStateFile(c.paths.State(), config.State{Device: cfg.Device, Hub: cfg.Hub, Version: version})
		client = hub.NewClient(cfg.Hub, token, version, reg, allowList, journal, state)
		client.Log = c.log
		runner = client.Runner()
		reps.add(client)
		client.OnUpdate = func(string) { updates.Hint() }
		tasks = append(tasks, c.hubTask(client))
	}
	if cfg.Has(config.RoleServe) {
		hubHealth := func() string {
			if client == nil {
				return "off"
			}
			return "on"
		}
		serve, err := c.serveTasks(ctx, cfg, reg, data, hubHealth, runner, reps, updates, agents)
		if err != nil {
			return err
		}
		tasks = append(tasks, serve...)
	}
	if len(tasks) == 0 {
		tasks = append(tasks, func(ctx context.Context) error { <-ctx.Done(); return nil })
	}

	done := make(chan error, len(tasks))
	for _, t := range tasks {
		go func() { done <- t(ctx) }()
	}
	select {
	case v := <-updates.Installed():
		c.log.Info("daemon updated, restart", "version", v)
		cancel()
		for range tasks {
			<-done
		}
		return restart(c.exe)
	case err = <-done:
	}
	cancel()
	if ctx.Err() != nil && err == nil {
		return nil
	}
	return err
}

// hubTask runs the hub client. After a refused token or hello, it waits, so that the service does not retry fast.
func (c *cli) hubTask(client *hub.Client) func(context.Context) error {
	return func(ctx context.Context) error {
		err := client.Run(ctx)
		if ctx.Err() != nil {
			return nil
		}
		var refused *hub.RefusedError
		if errors.Is(err, hub.ErrUnauthorized) || errors.As(err, &refused) {
			c.log.Error("hub stop", "err", err, "wait", stopWait)
			select {
			case <-ctx.Done():
			case <-time.After(stopWait):
			}
		}
		return err
	}
}

// reports passes the session reports of the oversire runtime to the sinks that the daemon makes after the runtime:
// the hub client, the session book and the direct orders. Without a sink, it drops them.
type reports struct {
	mu    sync.Mutex
	sinks []oversire.Reporter
}

func (r *reports) add(s oversire.Reporter) {
	r.mu.Lock()
	r.sinks = append(r.sinks, s)
	r.mu.Unlock()
}

func (r *reports) all() []oversire.Reporter {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.sinks)
}

func (r *reports) Report(s wire.RunnerSession) {
	for _, sink := range r.all() {
		sink.Report(s)
	}
}

func (r *reports) ReportPaths(order string, p wire.ChangedPaths) {
	for _, sink := range r.all() {
		if pr, ok := sink.(oversire.PathsReporter); ok {
			pr.ReportPaths(order, p)
		}
	}
}

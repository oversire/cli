package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"time"

	"github.com/oversire/cli/allow"
	"github.com/oversire/cli/ask"
	"github.com/oversire/cli/config"
	"github.com/oversire/cli/eventlog"
	"github.com/oversire/cli/group"
	"github.com/oversire/cli/hooks"
	"github.com/oversire/cli/orcawatch"
	"github.com/oversire/cli/orders"
	"github.com/oversire/cli/proc"
	"github.com/oversire/cli/questions"
	"github.com/oversire/cli/runtime"
	"github.com/oversire/cli/runtime/orca"
	"github.com/oversire/cli/secret"
	"github.com/oversire/cli/server"
	"github.com/oversire/cli/sessions"
	homestore "github.com/oversire/cli/store"
	"github.com/oversire/cli/term"
	"github.com/oversire/cli/update"
	"github.com/oversire/cli/wire"
	"github.com/oversire/cli/workers"
)

// spoolInterval is how often the daemon reads the hook spool.
const spoolInterval = 10 * time.Second

// serveTasks builds the serve role: the event log, the session book, the hooks, the Orca watcher, the group, the direct orders and the server.
// runner runs the direct orders. reports gets the book and the direct orders as sinks of the oversire runtime.
func (c *cli) serveTasks(ctx context.Context, cfg config.Config, reg *runtime.Registry, data string, hub func() string, runner *orders.Runner, reports *reports, updates *update.Loop) ([]func(context.Context) error, error) {
	serveCfg := cfg.ServeOrDefault()
	events := eventlog.New(eventlog.DefaultCapacity)
	book := sessions.NewBook(cfg.Machine(), filepath.Join(data, "sessions.json"), events, time.Now)
	runner.WithKeys(book)

	exe, ok := orca.FindCLI()
	if !ok {
		exe = orca.MacPath // Reachable is false, and the watcher waits and tries again
	}
	orcaCLI := orca.New(exe, c.cmds)
	// One driver for the registry, the watcher and the questions: it keeps the one coordinator of the workers.
	rt, hasOrca := reg.Get(orca.ID)
	if o, ok := rt.(*orca.Orca); ok {
		orcaCLI = o
	}
	watcher := orcawatch.NewWatcher(orcaCLI, book, nil)
	screen := orca.NewScreen(orcaCLI)
	userHome, _ := os.UserHomeDir()
	files := questions.NewFiles(filepath.Join(userHome, ".claude", "projects"), screen)
	asks := questions.NewChannel(book, screen, files, events, time.Now)
	workerBook := workers.NewBook(reg, allow.File{Path: c.paths.Allow()}, filepath.Join(data, "workers.json"), time.Now)
	if hasOrca {
		asks.WithMail(orcaCLI, workerBook.Terminals)
	}
	ingest := hooks.NewIngest(book, asks).WithHinter(watcher)
	installer := c.installer()
	spool := hooks.SpoolPath(data)
	if err := ingest.DrainSpool(spool); err != nil {
		c.log.Warn("hook spool", "err", err)
	}

	tailnet := server.NewTailnet(c.cmds)
	var srv *server.Server
	// A home override can run more daemons on one machine. Each keeps its group secret in its own folder.
	goos := goruntime.GOOS
	if config.Home() != "" {
		goos = ""
	}
	groupSecrets := secret.New(proc.Local{}, goos, secret.Group, filepath.Join(c.paths.Dir, "group.secret"), exec.LookPath)
	orcaCLI.WithPairing(orca.NewPairing(secret.New(proc.Local{}, goos, secret.Orca, filepath.Join(c.paths.Dir, "orca.pairing"), exec.LookPath)))
	store := group.NewStore(filepath.Join(c.paths.Dir, "group.json"), groupSecrets, time.Now, func() {
		if srv != nil {
			srv.Notify(false, map[string]string{"type": "group"})
		}
	})
	if err := store.Load(); err != nil {
		return nil, err
	}
	self := wire.GroupMember{Machine: cfg.Machine(), Host: cfg.Machine(), Addrs: []string{}, Port: serveCfg.ListenPort(), Roles: cfg.Roles, Version: version}
	if serveCfg.UseTailscale() {
		if addrs, err := tailnet.Addrs(ctx); err == nil {
			for _, a := range addrs {
				self.Addrs = append(self.Addrs, a.String())
			}
		}
	}
	if err := store.EnsureCreated(self); err != nil {
		return nil, err
	}
	joins := group.NewJoins(store, func(m any) {
		if srv != nil {
			srv.Notify(true, m)
		}
	}, time.Now)
	syncer := group.NewSyncer(store, tailnet.Peers, c.log)
	oplog, err := homestore.Open(filepath.Join(data, "store.db"), time.Now)
	if err != nil {
		return nil, err
	}
	storeRoutes := homestore.NewRoutes(oplog, store, func(m any) { srv.Notify(false, m) }, c.log)
	syncer.WithHomeTaker(storeRoutes)

	info := func() wire.DaemonInfo {
		d := wire.DaemonInfo{Machine: cfg.Machine(), Version: version, Contract: 1, Roles: cfg.Roles, Runtimes: reg.IDs(), Harnesses: installer.Status()}
		d.Group, d.Home = store.Info()
		d.Update = updates.Status()
		return d
	}
	srv = server.NewServer(serveCfg, info, store, events, book, ingest)
	srv.Log = c.log
	srv.Health = func() server.HealthSources {
		return server.HealthSources{Hooks: hookHealth(installer.Status()), Orca: watcher.Health(), Hub: hub()}
	}
	srv.Handle(joins.Routes()...)
	srv.Handle(syncer.Routes()...)
	srv.Handle(storeRoutes.Routes()...)
	srv.Handle(asks.Routes()...)
	srv.Handle(ask.NewAsker(book, screen, reg, time.Now).Routes()...)
	srv.Handle(installer.Routes()...)
	srv.Handle(updates.Routes()...)
	runtimeAPI := runtime.NewAPI(reg, filepath.Join(data, "files"), time.Now)
	runtimeAPI.Log = c.log
	srv.Handle(runtimeAPI.Routes()...)
	srv.Handle(workerBook.Routes()...)
	streams := term.NewAPI(book, reg, time.Now)
	streams.Log = c.log
	srv.Handle(streams.Routes()...)
	direct := orders.NewDirect(runner, func(m any) { srv.Notify(false, m) }, time.Now)
	direct.Log = c.log
	srv.Handle(direct.Routes()...)
	reports.add(book)
	reports.add(direct)

	drain := func(ctx context.Context) error {
		t := time.NewTicker(spoolInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-t.C:
				if err := ingest.DrainSpool(spool); err != nil {
					c.log.Warn("hook spool", "err", err)
				}
			}
		}
	}
	return []func(context.Context) error{srv.Run, book.Run, watcher.Run, syncer.Run, asks.Run, drain, c.pruneStore(oplog)}, nil
}

// hookHealth is "live" when a harness has our hooks, else "off".
func hookHealth(st []wire.HarnessHooks) string {
	for _, h := range st {
		if h.Hooks == wire.HooksInstalled {
			return "live"
		}
	}
	return "off"
}

// pruneStore drops old tombstones from the home store at the start and then each day.
func (c *cli) pruneStore(l *homestore.OpLog) func(context.Context) error {
	return func(ctx context.Context) error {
		t := time.NewTicker(24 * time.Hour)
		defer t.Stop()
		for {
			if err := l.Prune(time.Now()); err != nil {
				c.log.Warn("store prune", "err", err)
			}
			select {
			case <-ctx.Done():
				return nil
			case <-t.C:
			}
		}
	}
}

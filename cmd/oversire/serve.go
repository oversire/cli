package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"time"

	"github.com/oversire/cli/agents"
	"github.com/oversire/cli/agentsecret"
	"github.com/oversire/cli/appcall"
	"github.com/oversire/cli/ask"
	"github.com/oversire/cli/binding"
	"github.com/oversire/cli/capture"
	"github.com/oversire/cli/ci"
	"github.com/oversire/cli/config"
	"github.com/oversire/cli/eventlog"
	"github.com/oversire/cli/events"
	"github.com/oversire/cli/gitwatch"
	"github.com/oversire/cli/group"
	"github.com/oversire/cli/hooks"
	"github.com/oversire/cli/lease"
	"github.com/oversire/cli/link"
	"github.com/oversire/cli/membercall"
	"github.com/oversire/cli/mirror"
	"github.com/oversire/cli/orcawatch"
	"github.com/oversire/cli/orders"
	"github.com/oversire/cli/proc"
	"github.com/oversire/cli/questions"
	"github.com/oversire/cli/review"
	"github.com/oversire/cli/runbook"
	"github.com/oversire/cli/runtime"
	"github.com/oversire/cli/runtime/orca"
	"github.com/oversire/cli/secret"
	"github.com/oversire/cli/server"
	"github.com/oversire/cli/sessions"
	homestore "github.com/oversire/cli/store"
	"github.com/oversire/cli/term"
	"github.com/oversire/cli/update"
	"github.com/oversire/cli/wire"
	"github.com/oversire/cli/worker"
)

// spoolInterval is how often the daemon reads the hook spool.
const spoolInterval = 10 * time.Second

// serveTasks builds the serve role: the event log, the session book, the hooks, the Orca watcher, the group, the direct orders and the server.
// runner runs the direct orders. reports gets the book and the direct orders as sinks of the oversire runtime.
func (c *cli) serveTasks(ctx context.Context, cfg config.Config, reg *runtime.Registry, data string, hub func() string, runner *orders.Runner, reports *reports, updates *update.Loop, models agents.Lister, local *localSessions) ([]func(context.Context) error, error) {
	serveCfg := cfg.ServeOrDefault()
	rows, err := events.Open(filepath.Join(data, "events.db"), time.Now)
	if err != nil {
		return nil, err
	}
	hints := eventlog.New(eventlog.DefaultCapacity)
	if err := hints.Keep(ctx, rows); err != nil {
		c.log.Warn("events: read the kept hints", "err", err)
	}
	book := sessions.NewBook(cfg.Machine(), filepath.Join(data, "sessions.json"), hints, time.Now)
	runner.WithKeys(book)
	var runbooks *runbook.Engine
	places := events.NewResolver(rows, func(run string) string {
		if runbooks == nil {
			return ""
		}
		r, _ := runbooks.Get(run)
		return r.Item.Key
	})
	email, _, _ := c.cmds.Run(ctx, "git", []string{"config", "--global", "user.email"}, nil)
	capt := capture.NewWriter(rows, places, book, cfg.Machine(), strings.TrimSpace(string(email)), c.log)
	book.OnSend(capture.NewSessionRows(capt).OnSend)

	exe, ok := orca.FindCLI()
	if !ok {
		exe = orca.MacPath // Reachable is false, and the watcher waits and tries again
	}
	orcaCLI := orca.New(exe, c.cmds)
	// One driver for the registry, the watcher and the questions.
	rt, hasOrca := reg.Get(orca.ID)
	if o, ok := rt.(*orca.Orca); ok {
		orcaCLI = o
	}
	watcher := orcawatch.NewWatcher(orcaCLI, book, nil)
	// The watcher reads the last poll of the visor backend, so each poll gives a hint.
	local.OnOrcaPoll(watcher.Hint)
	screen := orca.NewScreen(orcaCLI)
	userHome, _ := os.UserHomeDir()
	files := questions.NewFiles(filepath.Join(userHome, ".claude", "projects"), screen)
	asks := questions.NewChannel(book, screen, files, hints, time.Now).WithEvents(capt)
	if hasOrca {
		go orcaCLI.RetireWorkers(ctx, filepath.Join(data, "workers.json"), c.log)
	}
	ingest := hooks.NewIngest(book, asks).WithHinter(watcher).WithEvents(capt)
	installer := c.installer()
	// An update can change the hook table. Install the Claude hooks again, so that the daemon and the hooks match.
	if c.home != "" {
		if done, err := installer.RefreshOutdated(); err != nil {
			c.log.Warn("hooks: refresh", "err", err)
		} else if len(done) > 0 {
			c.log.Info("hooks: installed again", "harnesses", done)
		}
	}
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
	store.Log = c.log
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
	// The agent secrets live on home. A daemon that is not home answers 409 and resolves through home.
	secretValues, err := agentsecret.NewStore(agentsecret.NewKeychain(proc.Local{}, goos, filepath.Join(data, "agent-secrets"), exec.LookPath),
		filepath.Join(data, "agent-secrets.json"), time.Now)
	if err != nil {
		return nil, err
	}
	secretHome := agentsecret.NewHome(secretValues, store, oplog, capt, time.Now)
	secretHome.Log = c.log
	bindings := binding.NewIndex(cfg.Machine(), c.log)
	bindings.Keep(filepath.Join(data, "worktree-bindings.json"))
	if err := bindings.Load(oplog); err != nil {
		c.log.Warn("worktree bindings", "err", err)
	}
	// OnApply keeps one callback.
	oplog.OnApply(func(ops []wire.StoreOp) {
		secretHome.Forget(ops)
		bindings.Apply(ops)
	})
	runner.WithEnv(agentsecret.NewEnv(store, secretHome, nil))
	storeRoutes := homestore.NewRoutes(oplog, store, func(m any) { srv.Notify(false, m) }, c.log)
	syncer.WithHomeTaker(storeRoutes)
	eventRoutes := events.NewRoutes(rows, func(m any) { srv.Notify(false, m) }, c.log)
	syncer.WithHomeTaker(eventRoutes)
	mirrors, err := mirror.Open(filepath.Join(data, "mirror.db"), time.Now)
	if err != nil {
		return nil, err
	}
	mirrors.Log = c.log
	syncer.WithHomeTaker(mirrors)
	ciBook, err := ci.Open(filepath.Join(data, "ci.db"), time.Now)
	if err != nil {
		return nil, err
	}
	ciIndex := ci.NewIndex(mirrors, ciBook, func() bool {
		src, ok := mirrors.Source()
		return ok && srv.HasCaller(src.Match)
	}, time.Now)
	ciIndex.Log = c.log
	outbox := events.NewOutbox(rows, store, nil, c.log)

	info := func() wire.DaemonInfo {
		d := wire.DaemonInfo{Machine: cfg.Machine(), Version: version, Contract: wire.DaemonContract, Roles: cfg.Roles, Runtimes: reg.IDs(), Harnesses: installer.Status()}
		d.Group, d.Home = store.Info()
		d.Update = updates.Status()
		d.Features = []string{wire.FeatureLaunchEnv, wire.FeatureAgentSecrets, wire.FeatureAppCalls, wire.FeatureMirror, wire.FeatureMemberCalls, wire.FeatureMirrorCI, wire.FeatureCI, wire.FeatureCIRedirect, wire.FeatureCIActions, wire.FeatureLinks}
		return d
	}
	srv = server.NewServer(serveCfg, info, store, hints, book, ingest)
	srv.Log = c.log
	// The drain publishes to srv, so it comes after srv.
	eventSpool := events.NewSpool(events.SpoolPath(data))
	if n, err := eventSpool.Drain(ctx, rows); err != nil {
		c.log.Warn("events: spool", "err", err)
	} else if n > 0 {
		c.log.Info("events: read the spool", "rows", n)
	}
	srv.Health = func() server.HealthSources {
		return server.HealthSources{Hooks: hookHealth(installer.Status()), Orca: watcher.Health(), Hub: hub()}
	}
	srv.Handle(joins.Routes()...)
	srv.Handle(syncer.Routes()...)
	srv.Handle(storeRoutes.Routes()...)
	srv.Handle(bindings.Routes()...)
	srv.Handle(secretHome.Routes()...)
	srv.Handle(eventRoutes.Routes()...)
	leases := lease.NewBook(filepath.Join(data, "leases.json"), time.Now)
	leases.OnChange = func(l wire.Lease) { srv.Notify(false, map[string]any{"type": "lease", "lease": l}) }
	srv.Handle(lease.NewRoutes(leases, store).Routes()...)
	srv.Handle(mirror.NewRoutes(mirrors, store, leases, store.Self, func(src mirror.Source) bool {
		return srv.HasCaller(src.Match)
	}, func(m any) { srv.Notify(false, m); ciIndex.Wake() }).Routes()...)
	srv.Handle(asks.Routes()...)
	appCalls := appcall.New(srv.Deliver, func() bool { return srv.HasClient(true) }, appcall.Wait)
	appCalls.Log = c.log
	srv.Handle(appCalls.Routes()...)
	members := membercall.New(srv.DeliverTo, mirrors, store, store.Self, appcall.Wait)
	members.Log = c.log
	srv.Handle(members.Routes()...)
	opener := link.NewOpener(srv, link.NewBook(time.Now), tailnet, store, capt, time.Now)
	opener.Log = c.log
	srv.Handle(opener.Routes()...)
	web := link.NewWebBase(tailnet, serveCfg.ListenPort(), serveCfg.UseTailscale(), os.Hostname)
	srv.Web = web.Get
	ciLimit := ci.NewLimiter(20*time.Second, time.Now)
	ciClient := &http.Client{Timeout: 330 * time.Second}
	ciFwd := ci.NewForward(store, ciClient)
	srv.Handle(ci.NewRoutes(ciIndex, ci.NewLogs(ciBook, ciIndex, members, ciLimit, store.Self), members, ciLimit, ciFwd, store.Self).Routes()...)
	srv.Handle(ci.NewExplainer(ciIndex, oplog, members, ci.NewLimiter(10*time.Minute, time.Now), store.Self).Routes(ciFwd)...)
	ciWatches := ci.NewWatches(ciIndex, srv.DeliverTo, mirrors, time.Now)
	ciWatches.Self = func() string { return store.Self().Machine }
	srv.Handle(ciWatches.Routes(ciFwd)...)
	asker := ask.NewAsker(book, screen, reg, time.Now)
	srv.Handle(asker.Routes()...)
	ciHome := ci.NewHomeClient(store, "http://"+net.JoinHostPort("127.0.0.1", strconv.Itoa(serveCfg.ListenPort())), ciClient)
	ciNotify := ci.NewNotifier(book, asker, ciHome, ciHome, filepath.Join(data, "ci"), time.Now)
	ciNotify.Log = c.log
	srv.Handle(ciNotify.Routes()...)
	srv.Handle(installer.Routes(hooks.NewSettings(c.paths))...)
	srv.Handle(updates.Routes()...)
	runtimeAPI := runtime.NewAPI(reg, filepath.Join(data, "files"), time.Now)
	runtimeAPI.Log = c.log
	srv.Handle(runtimeAPI.Routes()...)
	agentAPI := agents.NewAPI(models)
	agentAPI.Log = c.log
	srv.Handle(agentAPI.Routes()...)
	streams := term.NewAPI(book, reg, time.Now)
	streams.Log = c.log
	srv.Handle(streams.Routes()...)
	direct := orders.NewDirect(runner, func(m any) { srv.Notify(false, m) }, time.Now)
	direct.Log = c.log
	srv.Handle(direct.Routes()...)
	claims := orders.NewClaims(runner, capture.NewClaimRows(capt).OnClaim)
	claims.Log = c.log
	srv.Handle(claims.Routes()...)
	reports.add(book)
	reports.add(direct)
	runbookRows := capture.NewRunbookRows(capt)
	runbooks = runbook.NewEngine(runbook.NewStore(filepath.Join(data, "runbooks")), runner, reg, book, cfg.Machine(), func(r wire.RunbookRun) {
		runbookRows.OnChange(r)
		srv.Notify(false, map[string]any{"type": "runbookRun", "run": r})
	}, time.Now)
	runbooks.Log = c.log
	runbooks.Groups = bindings.GroupPath
	srv.Handle(runbook.NewRoutes(runbooks).Routes()...)
	workers := worker.NewEngine(worker.NewStore(filepath.Join(data, "worker-sessions")), book, reg, review.NewGit(c.cmds), runner, reg, capture.NewWorkerRows(capt).OnChange, time.Now)
	workers.Log = c.log
	srv.Handle(worker.NewRoutes(workers).Routes()...)
	var order review.Order
	if cfg.Review != nil {
		order = cfg.Review.Order
	}
	reviewStore := review.NewStore(filepath.Join(data, "reviews"))
	reviews := review.NewEngine(review.Deps{Store: reviewStore, Git: review.NewGit(c.cmds),
		Repos: reg, Sessions: book, Sender: asker, Launcher: runner, Picker: reg, Machine: cfg.Machine(), Order: order,
		Notify: func(ev wire.ReviewEvent) { srv.Notify(false, ev) }, Events: capt, Bindings: bindings})
	reviews.Log = c.log
	srv.Handle(review.NewRoutes(reviews).Routes()...)
	runtimeAPI.AddCapabilities(runtime.CapRunbooks, runtime.CapReviews)
	// The git watcher reads the repos of the runtimes and the folder roots of the reviews.
	gitRows := gitwatch.NewWatcher(review.NewGit(c.cmds), book, capt, places, func(ctx context.Context) []string {
		roots, _ := reviewStore.Roots()
		for _, r := range reg.Repos(ctx).Repos {
			roots = append(roots, r.Path)
		}
		return roots
	}, filepath.Join(data, "gitwatch.json"), c.log)
	srv.Handle(binding.NewScanner(gitRows, review.NewGit(c.cmds), book, time.Now).Routes()...)

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
				if _, err := eventSpool.Drain(ctx, rows); err != nil {
					c.log.Warn("events: spool", "err", err)
				}
			}
		}
	}
	return []func(context.Context) error{srv.Run, book.Run, watcher.Run, syncer.Run, asks.Run, drain, c.pruneStore(oplog), c.pruneMirror(mirrors), ciIndex.Run, ciWatches.Run, ciNotify.Resume, c.pruneCI(ciBook), runbooks.Run, reviews.Run, workers.Run,
		outbox.Run, hints.Run, capt.Run, gitRows.Run, web.Run, func(ctx context.Context) error { return rows.RunRetention(ctx, c.log) }}, nil
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

// pruneMirror drops old tombstones from the mirror at the start and then each hour.
func (c *cli) pruneMirror(s *mirror.Store) func(context.Context) error {
	return func(ctx context.Context) error {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			if err := s.Prune(time.Now()); err != nil {
				c.log.Warn("mirror prune", "err", err)
			}
			select {
			case <-ctx.Done():
				return nil
			case <-t.C:
			}
		}
	}
}

// pruneCI drops old CI runs and logs at the start and then each hour.
func (c *cli) pruneCI(b *ci.Book) func(context.Context) error {
	return func(ctx context.Context) error {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			if err := b.Prune(time.Now()); err != nil {
				c.log.Warn("ci prune", "err", err)
			}
			select {
			case <-ctx.Done():
				return nil
			case <-t.C:
			}
		}
	}
}

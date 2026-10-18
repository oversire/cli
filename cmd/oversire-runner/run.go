package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	goruntime "runtime"
	"strconv"
	"time"

	"github.com/oversire/runner/allow"
	"github.com/oversire/runner/config"
	"github.com/oversire/runner/hub"
	"github.com/oversire/runner/proc"
	"github.com/oversire/runner/runtime"
	"github.com/oversire/runner/runtime/orca"
	"github.com/oversire/runner/update"
)

// stopWait is the wait after a refused token or hello, so that the service does not retry fast.
const stopWait = time.Hour

func (c *cli) registry() *runtime.Registry {
	var rts []runtime.Runtime
	if exe, ok := orca.FindCLI(); ok {
		rts = append(rts, orca.New(exe, proc.Local{}))
	}
	return runtime.NewRegistry(rts...)
}

func (c *cli) run(ctx context.Context) error {
	cfg, err := c.mustConfig()
	if err != nil {
		return err
	}
	token, err := c.secrets.Get(ctx, cfg.Device)
	if err != nil {
		return &exitError{2, err}
	}
	if cfg.KeepAwake && goruntime.GOOS == "darwin" {
		if err := exec.Command("/usr/bin/caffeinate", "-s", "-w", strconv.Itoa(os.Getpid())).Start(); err != nil {
			c.log.Warn("caffeinate failed", "err", err)
		}
	}
	reg := c.registry()
	c.log.Info("runner start", "version", version, "hub", cfg.Hub, "device", cfg.Device, "runtimes", reg.IDs())

	state := config.NewStateFile(c.paths.State(), config.State{Device: cfg.Device, Hub: cfg.Hub, Version: version})
	client := hub.NewClient(cfg.Hub, token, version, reg, allow.File{Path: c.paths.Allow()}, hub.OpenJournal(c.paths.Journal()), state)
	client.Log = c.log

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	checks := make(chan struct{}, 1)
	client.OnUpdate = func(string) {
		select {
		case checks <- struct{}{}:
		default:
		}
	}
	updated := make(chan string, 1)
	go c.updateLoop(ctx, checks, updated)

	done := make(chan error, 1)
	go func() { done <- client.Run(ctx) }()
	select {
	case v := <-updated:
		c.log.Info("runner updated, restart", "version", v)
		cancel()
		<-done
		return restart(c.exe)
	case err = <-done:
	}
	if ctx.Err() != nil {
		return nil
	}
	var refused *hub.RefusedError
	if errors.Is(err, hub.ErrUnauthorized) || errors.As(err, &refused) {
		c.log.Error("runner stop", "err", err, "wait", stopWait)
		select {
		case <-ctx.Done():
		case <-time.After(stopWait):
		}
	}
	return err
}

// updateLoop checks the release feed one minute after the start, each day, and on each hub hint.
func (c *cli) updateLoop(ctx context.Context, hints <-chan struct{}, updated chan<- string) {
	u := update.NewUpdater(&http.Client{Timeout: 5 * time.Minute}, proc.Local{}, update.LatestURL, version, goruntime.GOOS, goruntime.GOARCH, c.exe)
	timer := time.NewTimer(time.Minute)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			timer.Reset(24 * time.Hour)
		case <-hints:
		}
		v, err := u.Check(ctx)
		if err != nil {
			c.log.Warn("update check failed", "err", err)
			continue
		}
		if v != "" {
			updated <- v
			return
		}
	}
}

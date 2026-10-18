package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"time"

	"github.com/oversire/runner/config"
	"github.com/oversire/runner/service"
)

type statusReport struct {
	Version   string         `json:"version"`
	Paired    bool           `json:"paired"`
	Device    string         `json:"device"`
	Hub       string         `json:"hub"`
	Connected bool           `json:"connected"`
	Since     *time.Time     `json:"since"`
	LastError string         `json:"lastError"`
	Service   service.Status `json:"service"`
	Runtimes  []string       `json:"runtimes"`
}

func (c *cli) status(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return &exitError{2, err}
	}
	cfg, paired, err := c.paths.LoadConfig()
	if err != nil {
		return err
	}
	st, err := config.LoadState(c.paths.State())
	if err != nil {
		return err
	}
	r := statusReport{
		Version: version, Paired: paired, Device: cfg.Device, Hub: cfg.Hub,
		Service: c.service.Status(ctx), Runtimes: c.registry().IDs(), LastError: st.LastError,
	}
	// A state file of a stopped service is old.
	if st.Connected && st.Device == cfg.Device && (r.Service.Running || !r.Service.Installed) {
		r.Connected, r.Since = true, st.Since
	}
	if *asJSON {
		b, _ := json.MarshalIndent(r, "", "  ")
		fmt.Fprintln(c.out, string(b))
		return nil
	}
	yesNo := map[bool]string{true: "yes", false: "no"}
	fmt.Fprintf(c.out, "version:   %s\n", r.Version)
	fmt.Fprintf(c.out, "paired:    %s\n", yesNo[r.Paired])
	if r.Paired {
		fmt.Fprintf(c.out, "device:    %s (%s)\n", r.Device, cfg.Name)
		fmt.Fprintf(c.out, "hub:       %s\n", r.Hub)
	}
	if r.Connected {
		fmt.Fprintf(c.out, "connected: yes, since %s\n", r.Since.Local().Format(time.DateTime))
	} else {
		fmt.Fprintln(c.out, "connected: no")
	}
	if r.LastError != "" {
		fmt.Fprintf(c.out, "error:     %s\n", r.LastError)
	}
	fmt.Fprintf(c.out, "service:   installed %s, running %s\n", yesNo[r.Service.Installed], yesNo[r.Service.Running])
	fmt.Fprintf(c.out, "runtimes:  %v\n", r.Runtimes)
	return nil
}

func (c *cli) install(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	keepAwake := fs.Bool("keep-awake", false, "keep the Mac awake while the runner runs (macOS)")
	if err := fs.Parse(args); err != nil {
		return &exitError{2, err}
	}
	cfg, err := c.mustConfig()
	if err != nil {
		return err
	}
	cfg.KeepAwake = *keepAwake
	if err := c.paths.SaveConfig(cfg); err != nil {
		return err
	}
	if err := c.service.Install(ctx); err != nil {
		return err
	}
	fmt.Fprintf(c.out, "The service is installed. It runs %s run at login.\n", c.exe)
	return nil
}

func (c *cli) uninstall(ctx context.Context) error {
	if err := c.service.Uninstall(ctx); err != nil {
		return err
	}
	fmt.Fprintln(c.out, "The service is removed.")
	return nil
}

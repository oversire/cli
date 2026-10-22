package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/oversire/cli/config"
	"github.com/oversire/cli/runtime/oversire"
	"github.com/oversire/cli/service"
	"github.com/oversire/cli/wire"
)

type statusReport struct {
	Version   string         `json:"version"`
	Roles     []string       `json:"roles"`
	Paired    bool           `json:"paired"`
	Device    string         `json:"device"`
	Hub       string         `json:"hub"`
	Connected bool           `json:"connected"`
	Since     *time.Time     `json:"since"`
	LastError string         `json:"lastError"`
	Service   service.Status `json:"service"`
	Runtimes  []string       `json:"runtimes"`
	Serve     serveStatus    `json:"serve"`
}

// serveStatus comes from GET /v1/whoami of the running daemon. It is empty when the daemon does not answer.
type serveStatus struct {
	Loopback  string             `json:"loopback"`
	Tailscale string             `json:"tailscale"`
	Group     string             `json:"group"`
	Home      string             `json:"home"`
	Update    *wire.UpdateStatus `json:"update,omitempty"`
}

func readServe(ctx context.Context, cfg config.Config) serveStatus {
	if !cfg.Has(config.RoleServe) {
		return serveStatus{}
	}
	url := "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.ServeOrDefault().ListenPort())) + "/v1/whoami"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return serveStatus{}
	}
	resp, err := (&http.Client{Timeout: time.Second}).Do(req)
	if err != nil {
		return serveStatus{}
	}
	defer resp.Body.Close()
	var d wire.DaemonInfo
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&d) != nil {
		return serveStatus{}
	}
	return serveStatus{Loopback: d.Listen.Loopback, Tailscale: d.Listen.Tailscale, Group: d.Group, Home: d.Home, Update: d.Update}
}

func (c *cli) status(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return &exitError{2, err}
	}
	cfg, err := c.setup(ctx)
	if err != nil {
		return err
	}
	st, err := config.LoadState(c.paths.State())
	if err != nil {
		return err
	}
	r := statusReport{
		Version: version, Roles: cfg.Roles, Paired: cfg.Paired(), Device: cfg.Device, Hub: cfg.Hub,
		Service: c.service.Status(ctx), Runtimes: c.registry(oversire.New(oversire.Options{}, nil, nil, nil)).IDs(), LastError: st.LastError,
		Serve: readServe(ctx, cfg),
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
	fmt.Fprintf(c.out, "roles:     %v\n", r.Roles)
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
	if r.Serve.Loopback != "" {
		fmt.Fprintf(c.out, "serve:     %s", r.Serve.Loopback)
		if r.Serve.Tailscale != "" {
			fmt.Fprintf(c.out, ", %s", r.Serve.Tailscale)
		}
		fmt.Fprintln(c.out)
	}
	if r.Serve.Group != "" {
		fmt.Fprintf(c.out, "group:     %s (home %s)\n", r.Serve.Group, r.Serve.Home)
	}
	if u := r.Serve.Update; u != nil {
		line := "latest " + u.Latest
		if u.Error != "" {
			line = "failed: " + u.Error
		}
		fmt.Fprintf(c.out, "update:    %s (checked %s)\n", line, u.CheckedAt.Local().Format(time.DateTime))
	}
	return nil
}

func (c *cli) install(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	keepAwake := fs.Bool("keep-awake", false, "keep the Mac awake while the daemon runs (macOS)")
	if err := fs.Parse(args); err != nil {
		return &exitError{2, err}
	}
	cfg, err := c.setup(ctx)
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
	fmt.Fprintf(c.out, "The service is installed. It runs %s daemon run at login.\n", c.exe)
	return nil
}

func (c *cli) uninstall(ctx context.Context) error {
	if err := c.service.Uninstall(ctx); err != nil {
		return err
	}
	fmt.Fprintln(c.out, "The service is removed.")
	return nil
}

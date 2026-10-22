package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	goruntime "runtime"
	"strconv"
	"time"

	"github.com/oversire/cli/config"
	"github.com/oversire/cli/proc"
	"github.com/oversire/cli/update"
	"github.com/oversire/cli/wire"
)

// errNoRoute: the daemon runs a CLI without POST /v1/update.
var errNoRoute = errors.New("the daemon has no update route")

// update asks the running daemon to update now. Without a daemon, or with an old one, it updates the binary itself.
func (c *cli) update(ctx context.Context) error {
	cfg, err := c.setup(ctx)
	if err != nil {
		return err
	}
	s, err := askUpdate(ctx, cfg)
	if err != nil {
		r, err := update.NewUpdater(&http.Client{Timeout: 5 * time.Minute}, proc.Local{}, update.LatestURL, version, goruntime.GOOS, goruntime.GOARCH, c.exe).Check(ctx)
		s = wire.UpdateStatus{Latest: r.Latest, Installed: r.Installed}
		if err != nil {
			s.Error = err.Error()
		}
		if s.Installed != "" && c.service.Status(ctx).Running {
			if err := c.service.Install(ctx); err != nil {
				s.Error = "the daemon did not restart: " + err.Error()
			}
		}
	}
	switch {
	case s.Error != "":
		return &exitError{1, errors.New(s.Error)}
	case s.Installed != "":
		fmt.Fprintf(c.out, "Updated to %s. The daemon restarts.\n", s.Installed)
	default:
		fmt.Fprintf(c.out, "oversire %s is the latest.\n", version)
	}
	return nil
}

func askUpdate(ctx context.Context, cfg config.Config) (wire.UpdateStatus, error) {
	var s wire.UpdateStatus
	if !cfg.Has(config.RoleServe) {
		return s, errNoRoute
	}
	url := "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.ServeOrDefault().ListenPort())) + "/v1/update"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return s, err
	}
	resp, err := (&http.Client{Timeout: 6 * time.Minute}).Do(req)
	if err != nil {
		return s, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return s, errNoRoute
	}
	return s, json.NewDecoder(resp.Body).Decode(&s)
}

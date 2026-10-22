package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/oversire/cli/group"
	"github.com/oversire/cli/wire"
)

const groupUsage = `Usage:
  oversire group show [--json]               show the machines and apps of the group
  oversire group invite                      make a join code for a new machine
  oversire join <host> <code>|-              join the group of the member at host
  oversire group requests                    list the open join requests
  oversire group approve <id>                approve a join request
  oversire group deny <id>                   deny a join request
  oversire group request-code                make a code that a member uses to add this machine
  oversire group admit <machine> <code>      add the machine that shows code
  oversire group remove <machine|deviceId>   remove a machine or an app
  oversire group home <machine>              move home to a member
  oversire group rotate                      make a new group secret
`

// daemonClient calls the loopback API of the local daemon.
type daemonClient struct {
	base string
	http *http.Client
}

func (c *cli) daemonClient() (daemonClient, error) {
	cfg, _, err := c.paths.LoadConfig()
	if err != nil {
		return daemonClient{}, err
	}
	port := cfg.ServeOrDefault().ListenPort()
	return daemonClient{base: "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), http: &http.Client{Timeout: 30 * time.Second}}, nil
}

// call sends body as JSON and decodes the answer into out.
func (d daemonClient) call(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, d.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.http.Do(req)
	if err != nil {
		return fmt.Errorf("The daemon does not answer on %s. Start it with: oversire daemon install", strings.TrimPrefix(d.base, "http://"))
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return errors.New(e.Error)
		}
		return fmt.Errorf("The daemon answered %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("The daemon answer is not valid: %w", err)
		}
	}
	return nil
}

func (c *cli) group(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return &exitError{2, errors.New(groupUsage)}
	}
	d, err := c.daemonClient()
	if err != nil {
		return err
	}
	sub, rest := args[0], args[1:]
	need := func(n int, form string) error {
		if len(rest) != n {
			return &exitError{2, errors.New("usage: oversire group " + form)}
		}
		return nil
	}
	switch sub {
	case "show":
		return c.groupShow(ctx, d, rest)
	case "invite":
		if err := need(0, "invite"); err != nil {
			return err
		}
		var inv wire.GroupInvite
		if err := d.call(ctx, http.MethodPost, "/v1/group/invites", struct{}{}, &inv); err != nil {
			return err
		}
		fmt.Fprintf(c.out, "%s · %s\nOn the new machine, run: oversire join %s %s\nThe code is good for one use until %s.\n",
			inv.Via, inv.Code, inv.Via, inv.Code, inv.ExpiresAt.Local().Format(time.TimeOnly))
		return nil
	case "requests":
		if err := need(0, "requests"); err != nil {
			return err
		}
		var list []group.Pending
		if err := d.call(ctx, http.MethodGet, "/v1/group/requests", nil, &list); err != nil {
			return err
		}
		if len(list) == 0 {
			fmt.Fprintln(c.out, "No open join requests.")
			return nil
		}
		for _, p := range list {
			fmt.Fprintf(c.out, "%s  %s (%s, %s)  code %s  until %s\n", p.Request, p.Machine, p.Host, p.Kind, p.Confirm,
				p.ExpiresAt.Local().Format(time.TimeOnly))
		}
		return nil
	case "approve", "deny":
		if err := need(1, sub+" <id>"); err != nil {
			return err
		}
		if err := d.call(ctx, http.MethodPost, "/v1/group/requests/"+url.PathEscape(rest[0])+"/"+sub, struct{}{}, nil); err != nil {
			return err
		}
		fmt.Fprintf(c.out, "The request %s is %s.\n", rest[0], map[string]string{"approve": "approved", "deny": "denied"}[sub])
		return nil
	case "request-code":
		if err := need(0, "request-code"); err != nil {
			return err
		}
		var inv wire.GroupInvite
		if err := d.call(ctx, http.MethodPost, "/v1/group/offers", struct{}{}, &inv); err != nil {
			return err
		}
		fmt.Fprintf(c.out, "%s · %s\nOn a member, run: oversire group admit %s %s\nThe code is good for one use until %s.\n",
			inv.Via, inv.Code, inv.Via, inv.Code, inv.ExpiresAt.Local().Format(time.TimeOnly))
		return nil
	case "admit":
		if err := need(2, "admit <machine> <code>"); err != nil {
			return err
		}
		if err := d.call(ctx, http.MethodPost, "/v1/group/admit", map[string]string{"host": rest[0], "code": rest[1]}, nil); err != nil {
			return err
		}
		fmt.Fprintf(c.out, "%s joined the group.\n", rest[0])
		return nil
	case "remove":
		if err := need(1, "remove <machine|deviceId>"); err != nil {
			return err
		}
		return c.groupRemove(ctx, d, rest[0])
	case "home":
		if err := need(1, "home <machine>"); err != nil {
			return err
		}
		if err := d.call(ctx, http.MethodPost, "/v1/group/home", map[string]string{"machine": rest[0]}, nil); err != nil {
			return err
		}
		fmt.Fprintf(c.out, "Home is now %s.\n", rest[0])
		return nil
	case "rotate":
		if err := need(0, "rotate"); err != nil {
			return err
		}
		if err := d.call(ctx, http.MethodPost, "/v1/group/rotate", struct{}{}, nil); err != nil {
			return err
		}
		fmt.Fprintf(c.out, "The group has a new secret. The members get it with the next sync. The old secret stays valid for %s.\n", group.RotateGrace)
		return nil
	}
	return &exitError{2, fmt.Errorf("unknown group command %q\n\n%s", sub, groupUsage)}
}

func (c *cli) groupShow(ctx context.Context, d daemonClient, args []string) error {
	fs := flag.NewFlagSet("group show", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return &exitError{2, err}
	}
	var g wire.Group
	if err := d.call(ctx, http.MethodGet, "/v1/group", nil, &g); err != nil {
		return err
	}
	if *asJSON {
		b, _ := json.MarshalIndent(g, "", "  ")
		fmt.Fprintln(c.out, string(b))
		return nil
	}
	fmt.Fprintf(c.out, "%s (group %s, version %d)\n", g.Name, g.ID, g.Version)
	fmt.Fprintln(c.out, "machines:")
	for _, m := range g.Members {
		mark := ""
		if m.Machine == g.Home {
			mark = "  home"
		}
		seen := ""
		if m.LastSeen != nil {
			seen = "  seen " + m.LastSeen.Local().Format(time.DateTime)
		}
		fmt.Fprintf(c.out, "  %s  %s%s%s\n", m.Machine, group.Endpoint(m.Host, m.Port), mark, seen)
	}
	apps := 0
	for _, dev := range g.Devices {
		if dev.Kind == wire.DeviceApp {
			if apps == 0 {
				fmt.Fprintln(c.out, "apps:")
			}
			apps++
			fmt.Fprintf(c.out, "  %s  %s\n", dev.DeviceID, dev.Name)
		}
	}
	if g.Cloud != nil {
		fmt.Fprintf(c.out, "cloud: %s\n", g.Cloud.Hub)
	}
	return nil
}

// groupRemove removes a member by machine name, else a device by id.
func (c *cli) groupRemove(ctx context.Context, d daemonClient, target string) error {
	var g wire.Group
	if err := d.call(ctx, http.MethodGet, "/v1/group", nil, &g); err != nil {
		return err
	}
	path := "/v1/group/devices/" + url.PathEscape(target)
	for _, m := range g.Members {
		if m.Machine == target {
			path = "/v1/group/members/" + url.PathEscape(target)
		}
	}
	if err := d.call(ctx, http.MethodDelete, path, nil, nil); err != nil {
		return err
	}
	fmt.Fprintf(c.out, "%s is removed. Its credentials are revoked.\n", target)
	return nil
}

// join joins the group of the member at host with a code. The code "-" reads one line from stdin.
func (c *cli) join(ctx context.Context, args []string) error {
	if len(args) != 2 {
		return &exitError{2, errors.New("usage: oversire join <host> <code>|-")}
	}
	host, code := args[0], args[1]
	if code == "-" {
		line, err := bufio.NewReader(c.in).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		code = line
	}
	code = strings.TrimSpace(code)
	if code == "" {
		return &exitError{2, errors.New("The code is empty.")}
	}
	d, err := c.daemonClient()
	if err != nil {
		return err
	}
	var g wire.Group
	if err := d.call(ctx, http.MethodPost, "/v1/group/enter", map[string]string{"host": host, "code": code}, &g); err != nil {
		return err
	}
	fmt.Fprintf(c.out, "Joined %s (group %s) through %s. Machines: %d.\n", g.Name, g.ID, host, len(g.Members))
	return nil
}

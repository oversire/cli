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
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/oversire/cli/config"
)

type pairResult struct {
	Token     string `json:"token"`
	DeviceID  string `json:"deviceId"`
	Role      string `json:"role"`
	Account   string `json:"account"`
	HookRoute string `json:"hookRoute"`
}

// pair accepts flags before and after the code. The code "-" reads one line from stdin.
func (c *cli) pair(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("pair", flag.ContinueOnError)
	hub := fs.String("hub", "", "the hub URL, for example https://hub.example.com")
	name := fs.String("name", "", "the name of this runner in the console (default: the host name)")
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return &exitError{2, err}
		}
		if fs.NArg() == 0 {
			break
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if len(positional) != 1 {
		return &exitError{2, errors.New("usage: oversire-runner pair <code>|- [--hub URL] [--name NAME]")}
	}
	code := positional[0]
	if code == "-" {
		line, err := bufio.NewReader(c.in).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		code = line
	}
	code = strings.ToLower(strings.TrimSpace(code))
	if code == "" {
		return &exitError{2, errors.New("The code is empty.")}
	}

	old, _, _ := c.paths.LoadConfig()
	if *hub == "" {
		*hub = os.Getenv("OVERSIRE_HUB")
	}
	if *hub == "" {
		*hub = old.Hub
	}
	if *hub == "" {
		return &exitError{2, errors.New("Give the hub URL with --hub.")}
	}
	*hub = strings.TrimRight(*hub, "/")
	if *name == "" {
		*name, _ = os.Hostname()
	}

	res, err := postPair(ctx, *hub, code, *name)
	if err != nil {
		return err
	}
	if res.Role != "runner" {
		return errors.New("This code is for a client. Make a runner code in the console.")
	}
	if res.Token == "" || res.DeviceID == "" {
		return errors.New("The hub gave no token or device id.")
	}
	if old.Device != "" && old.Device != res.DeviceID {
		c.secrets.Delete(ctx, old.Device)
	}
	if err := c.secrets.Set(ctx, res.DeviceID, res.Token); err != nil {
		return fmt.Errorf("store the token: %w", err)
	}
	cfg := config.Config{Hub: *hub, Device: res.DeviceID, Account: res.Account, Name: *name, KeepAwake: old.KeepAwake,
		MaxSessions: old.MaxSessions, DataDir: old.DataDir}
	if err := c.paths.SaveConfig(cfg); err != nil {
		return err
	}
	if err := c.service.Restart(ctx); err != nil {
		c.log.Warn("service restart failed", "err", err)
	}
	fmt.Fprintf(c.out, "Paired as %s (device %s). The token is in the %s.\n", cfg.Name, cfg.Device, c.secrets.Where())
	return nil
}

func postPair(ctx context.Context, hub, code, name string) (pairResult, error) {
	var res pairResult
	body, _ := json.Marshal(map[string]string{"code": code, "name": name})
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, hub+"/pair", bytes.NewReader(body))
	if err != nil {
		return res, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return res, fmt.Errorf("The hub %s is not reachable: %w", hub, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusBadRequest:
		return res, errors.New("The code is not valid. A code has 8 letters and digits, for example ABCD-1234.")
	case http.StatusUnauthorized:
		return res, errors.New("The code is unknown or old. Make a new runner code in the console.")
	default:
		return res, fmt.Errorf("The hub answered %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	if err := json.Unmarshal(data, &res); err != nil {
		return res, fmt.Errorf("The hub answer is not valid: %w", err)
	}
	return res, nil
}

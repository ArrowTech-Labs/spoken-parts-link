// Package config reads the link's settings from the environment and the
// add-on options file.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/ArrowTech-Labs/spoken-parts-link/internal/api"
)

// Defaults.
const (
	DefaultDataDir = "/data"
	// DefaultListen is the Supervisor side address of the add-on with
	// host_network. Binding here keeps the page off the LAN.
	DefaultListen = "172.30.32.1:8099"
	// DefaultAllow is the Supervisor ingress proxy, the only allowed caller.
	DefaultAllow = "172.30.32.2"
	DefaultSSDP  = "0.0.0.0:2021"
)

// Config is the link's runtime configuration.
type Config struct {
	DataDir  string
	Server   *url.URL
	Listen   string
	Allow    []netip.Addr
	SSDP     string
	LogLevel string
	// AutoStart lets the link start prints itself on Developer Mode printers
	// (project_file). Off unless SP_LINK_AUTOSTART is "true".
	AutoStart bool
}

// Load reads env (through getenv) and <data>/options.json.
//
//	SP_LINK_SERVER  backend base URL (tests and development only)
//	SP_LINK_LISTEN  web listen address, default 172.30.32.1:8099
//	SP_LINK_ALLOW   comma separated source IPs allowed, default 172.30.32.2
//	SP_LINK_DATA    data folder, default /data
//	SP_LINK_SSDP    discovery listen address, default 0.0.0.0:2021
//	SP_LINK_AUTOSTART  "true" to allow auto start (Developer Mode printers only), default off
func Load(getenv func(string) string) (Config, error) {
	c := Config{
		DataDir:   or(getenv("SP_LINK_DATA"), DefaultDataDir),
		Listen:    or(getenv("SP_LINK_LISTEN"), DefaultListen),
		SSDP:      or(getenv("SP_LINK_SSDP"), DefaultSSDP),
		LogLevel:  "info",
		AutoStart: strings.EqualFold(strings.TrimSpace(getenv("SP_LINK_AUTOSTART")), "true"),
	}
	srv, err := api.ParseServer(or(getenv("SP_LINK_SERVER"), api.DefaultServer))
	if err != nil {
		return c, err
	}
	c.Server = srv
	for _, s := range strings.Split(or(getenv("SP_LINK_ALLOW"), DefaultAllow), ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		a, err := netip.ParseAddr(s)
		if err != nil {
			return c, fmt.Errorf("SP_LINK_ALLOW: %q is not an IP address", s)
		}
		c.Allow = append(c.Allow, a.Unmap())
	}
	if len(c.Allow) == 0 {
		return c, errors.New("SP_LINK_ALLOW is empty")
	}
	var opts struct {
		LogLevel string `json:"log_level"`
	}
	if b, err := os.ReadFile(filepath.Join(c.DataDir, "options.json")); err == nil {
		if json.Unmarshal(b, &opts) == nil && opts.LogLevel != "" {
			c.LogLevel = opts.LogLevel
		}
	}
	return c, nil
}

func or(v, d string) string {
	if strings.TrimSpace(v) == "" {
		return d
	}
	return strings.TrimSpace(v)
}

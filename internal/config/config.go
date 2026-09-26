// Copyright 2026 Yauhen Bichel
// SPDX-License-Identifier: Apache-2.0

// Package config reads the optional TOML file. Flags on the command line override it; the environment
// variable LLM_HOPS_TOKEN overrides the token, so a secret never has to sit in a file or a unit.
package config

import (
	"fmt"
	"os"

	"github.com/BurntSushi/toml"
)

// Config is everything `llm-hops serve` needs.
type Config struct {
	Listen    string   `toml:"listen"`     // address to listen on
	DB        string   `toml:"db"`         // the SQLite file
	KeepDays  float64  `toml:"keep_days"`  // spans older than this are rolled up into daily statistics and dropped
	Tail      []string `toml:"tail"`       // yllm-gateway request logs to follow
	FromStart bool     `toml:"from_start"` // read the whole log first, then follow
	Token     string   `toml:"token"`      // when set, the API and the page need it (bearer header or cookie)
	Title     string   `toml:"title"`      // the page's title
	Demo      int      `toml:"demo"`       // synthetic traces to load at start
	Live      bool     `toml:"live"`       // with demo: keep adding one trace a second
}

// Default is what runs with no file and no flags.
func Default() Config {
	return Config{Listen: "127.0.0.1:11602", DB: "hops.db", KeepDays: 14, Title: "llm-hops"}
}

// Load reads path over the defaults; a missing path with `must` false is not an error.
func Load(path string, must bool) (Config, error) {
	c := Default()
	if path == "" {
		return applyEnv(c), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) && !must {
			return applyEnv(c), nil
		}
		return c, err
	}
	md, err := toml.Decode(string(data), &c)
	if err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		return c, fmt.Errorf("%s: unknown key %q", path, undecoded[0].String())
	}
	if c.KeepDays <= 0 {
		return c, fmt.Errorf("%s: keep_days must be positive", path)
	}
	return applyEnv(c), nil
}

func applyEnv(c Config) Config {
	if t := os.Getenv("LLM_HOPS_TOKEN"); t != "" {
		c.Token = t
	}
	return c
}

// Example is a documented file a user can start from.
const Example = `# llm-hops configuration. Every key is optional; flags on the command line win over this file.
listen = "127.0.0.1:11602"
db = "hops.db"
keep_days = 14            # spans older than this become daily statistics and are dropped
tail = []                 # yllm-gateway request logs to follow, e.g. ["/home/me/yllm-gateway/var/requests.jsonl"]
from_start = false        # read the whole log first, then follow
# token = "..."           # when set, POST /api/v1/spans, the API and the page need it
                          # (Authorization: Bearer, or open the page once as /#token=...); LLM_HOPS_TOKEN overrides
title = "llm-hops"
`

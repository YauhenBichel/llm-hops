// Copyright 2026 Yauhen Bichel
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTheExampleFileLoads(t *testing.T) {
	p := filepath.Join(t.TempDir(), "hops.toml")
	os.WriteFile(p, []byte(Example), 0o600)
	c, err := Load(p, true)
	if err != nil || c.Listen != "127.0.0.1:11602" || c.KeepDays != 14 {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestDefaultsFileAndEnvironment(t *testing.T) {
	c, err := Load("", false)
	if err != nil || c.Listen != "127.0.0.1:11602" || c.KeepDays != 14 || c.DB != "hops.db" {
		t.Fatalf("%+v %v", c, err)
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "hops.toml")
	os.WriteFile(p, []byte("tail = [\"/var/log/a.jsonl\"]\nkeep_days = 30\ntoken = \"filetoken\"\n"), 0o600)
	c, err = Load(p, true)
	if err != nil || len(c.Tail) != 1 || c.KeepDays != 30 || c.Token != "filetoken" || c.Title != "llm-hops" {
		t.Fatalf("%+v %v", c, err)
	}
	t.Setenv("LLM_HOPS_TOKEN", "envtoken")
	c, _ = Load(p, true)
	if c.Token != "envtoken" {
		t.Fatal("the environment must win for the token")
	}
	if _, err := Load(filepath.Join(dir, "missing.toml"), true); err == nil {
		t.Fatal("a required file that is missing must fail")
	}
	if c, err := Load(filepath.Join(dir, "missing.toml"), false); err != nil || c.Listen == "" {
		t.Fatal("an optional file that is missing is fine")
	}
	os.WriteFile(p, []byte("listen = \"x\"\nbogus = 1\n"), 0o600)
	if _, err := Load(p, true); err == nil {
		t.Fatal("an unknown key must fail, so a typo is not silently ignored")
	}
	os.WriteFile(p, []byte("keep_days = 0\n"), 0o600)
	if _, err := Load(p, true); err == nil {
		t.Fatal("keep_days 0 must fail")
	}
}

func TestTheLimitsHaveDefaultsAndAreChecked(t *testing.T) {
	c, err := Load("", false)
	if err != nil || c.MaxSpansPerTrace != 5000 || c.MaxDBMB != 0 {
		t.Fatalf("%+v %v", c, err)
	}
	p := filepath.Join(t.TempDir(), "hops.toml")
	os.WriteFile(p, []byte("max_spans_per_trace = 0\nmax_db_mb = 2048\n"), 0o600)
	if c, err = Load(p, true); err != nil || c.MaxSpansPerTrace != 0 || c.MaxDBMB != 2048 {
		t.Fatalf("%+v %v", c, err)
	}
	os.WriteFile(p, []byte("max_spans_per_trace = -1\n"), 0o600)
	if _, err = Load(p, true); err == nil {
		t.Fatal("a negative cap loaded")
	}
}

package main

import (
	"strings"
	"testing"

	"github.com/sensu/sensu-plugin-sdk/sensu"
)

func TestCheckArgs(t *testing.T) {
	valid := func() Config {
		return Config{
			StateGlobs: []string{"/home/deploy/*/shared/tmp/pids/puma.state,/var/www/*/shared/tmp/pids/puma.state"},
			Controls:   []string{"9294, 9293", "tcp://127.0.0.1:9293"},
			Expect:     []string{"myapp_prod, *_api"},
			MinWorkers: 2, BusyWarn: 80, BusyCrit: 95, Timeout: 5,
		}
	}
	tests := []struct {
		name    string
		mutate  func(c *Config)
		wantErr string
	}{
		{name: "valid"},
		{name: "state glob or control required", mutate: func(c *Config) { c.StateGlobs, c.Controls = []string{""}, []string{" "} }, wantErr: "--state-glob or --control is required"},
		{name: "control without state glob", mutate: func(c *Config) { c.StateGlobs, c.Controls = []string{"none"}, []string{"9293"} }, wantErr: "-"},
		{name: "none for both", mutate: func(c *Config) { c.StateGlobs, c.Controls = []string{"none"}, []string{"none"} }, wantErr: "--state-glob or --control is required"},
		{name: "invalid control", mutate: func(c *Config) { c.Controls = []string{"9293-9295"} }, wantErr: "invalid --control"},
		{name: "invalid state glob", mutate: func(c *Config) { c.StateGlobs = []string{"/home/[/x"} }, wantErr: "invalid --state-glob"},
		{name: "invalid expect pattern", mutate: func(c *Config) { c.Expect = []string{"myapp_[prod"} }, wantErr: "invalid --expect"},
		{name: "negative threshold", mutate: func(c *Config) { c.RSSCrit = -1 }, wantErr: "--rss-crit must not be negative"},
		{name: "zero timeout", mutate: func(c *Config) { c.Timeout = 0 }, wantErr: "--timeout must be greater than 0"},
	}
	saved := plugin
	t.Cleanup(func() { plugin = saved })

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plugin = valid()
			if tt.mutate != nil {
				tt.mutate(&plugin)
			}
			state, err := checkArgs(nil)
			if tt.wantErr == "-" { // valid, other lists
				if err != nil || state != sensu.CheckStateOK || len(plugin.StateGlobs) != 0 {
					t.Fatalf("checkArgs() = %d, %v, globs %q", state, err, plugin.StateGlobs)
				}
				return
			}
			if tt.wantErr == "" {
				if err != nil || state != sensu.CheckStateOK {
					t.Fatalf("checkArgs() = %d, %v", state, err)
				}
				// Comma separated lists, as set through environment variables, are split.
				if len(plugin.StateGlobs) != 2 || strings.Join(plugin.Expect, "|") != "myapp_prod|*_api" {
					t.Errorf("globs = %q, expect = %q", plugin.StateGlobs, plugin.Expect)
				}
				// Ports become URLs, duplicates dropped.
				if got := strings.Join(plugin.Controls, "|"); got != "tcp://127.0.0.1:9293|tcp://127.0.0.1:9294" {
					t.Errorf("controls = %q", got)
				}
				return
			}
			if state != sensu.CheckStateUnknown || err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("checkArgs() = %d, %v, want UNKNOWN with %q", state, err, tt.wantErr)
			}
		})
	}
}

func TestSplitList(t *testing.T) {
	got := splitList([]string{"myapp_prod, myapp_staging", "", "other_prod"})
	want := []string{"myapp_prod", "myapp_staging", "other_prod"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("splitList() = %v, want %v", got, want)
	}
	if got := splitList([]string{""}); got != nil {
		t.Errorf("splitList(empty) = %v, want nil", got)
	}
}

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const btime = 1_700_000_000

// fakeProc writes a minimal procfs: pid → process title.
func fakeProc(t *testing.T, procs map[int]string) string {
	t.Helper()
	root := t.TempDir()
	write := func(path, content string) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root, "stat"), fmt.Sprintf("cpu  1 2 3\nbtime %d\nprocesses 42\n", btime))
	write(filepath.Join(root, "self", "stat"), "not a pid dir")
	for pid, title := range procs {
		dir := filepath.Join(root, fmt.Sprint(pid))
		// Puma rewrites argv: title followed by NUL padding.
		write(filepath.Join(dir, "cmdline"), title+strings.Repeat("\x00", 8))
		// starttime (field 22) = pid seconds after boot, comm with a space and parenthesis.
		write(filepath.Join(dir, "stat"), fmt.Sprintf("%d (ruby (x)) S 1 %s %d 0", pid, strings.Repeat("0 ", 17), pid*clockTicks))
		write(filepath.Join(dir, "status"), fmt.Sprintf("Name:\truby\nVmRSS:\t   %d kB\nThreads:\t5\n", pid))
	}
	return root
}

func proc(pid int) Process {
	return Process{PID: pid, StartedAt: time.Unix(btime+int64(pid), 0), RSS: uint64(pid) * 1024}
}

func TestDiscover(t *testing.T) {
	root := fakeProc(t, map[int]string{
		100: "puma 6.4.2 (unix:///var/www/my_app_prod/shared/tmp/sockets/puma.sock) [my_app_prod]",
		101: "puma: cluster worker 0: 100 [my_app_prod]",
		102: "puma: cluster worker 1: 100 [my_app_prod]",
		200: "puma 6.4.2 (tcp://0.0.0.0:3001) [myapp_staging]",
		201: "puma: cluster worker 0: 200 [myapp_staging]",
		300: "/usr/sbin/nginx -g daemon off;",
		400: "ruby bin/puma -C config/puma.rb --tag myapp_prod", // booting, title not set yet
	})

	got, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []*Instance{
		{
			Tag: "my_app_prod", App: "my_app", Env: "prod",
			Masters: []Process{proc(100)},
			Workers: []Worker{{proc(101), 0, 100}, {proc(102), 1, 100}},
		},
		{
			Tag: "myapp_staging", App: "myapp", Env: "staging",
			Masters: []Process{proc(200)},
			Workers: []Worker{{proc(201), 0, 200}},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Discover() =\n%+v\nwant\n%+v", deref(got), deref(want))
	}
}

func TestDiscoverNoProc(t *testing.T) {
	if _, err := Discover(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("Discover() on a missing procfs: want an error")
	}
}

func TestSplitTag(t *testing.T) {
	for tag, want := range map[string][2]string{
		"myapp_prod":   {"myapp", "prod"},
		"my_app_stage": {"my_app", "stage"},
		"myapp":        {"myapp", ""},
		"_prod":        {"_prod", ""},
		"myapp_":       {"myapp_", ""},
	} {
		app, env := SplitTag(tag)
		if app != want[0] || env != want[1] {
			t.Errorf("SplitTag(%q) = %q, %q, want %q, %q", tag, app, env, want[0], want[1])
		}
	}
}

func deref(is []*Instance) []Instance {
	out := make([]Instance, len(is))
	for i, in := range is {
		out[i] = *in
	}
	return out
}

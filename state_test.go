package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opsone/sensu-plugins-puma/internal/puma"
	"github.com/sensu/sensu-plugin-sdk/sensu"
)

func writeFile(t *testing.T, path, content string, perm os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), perm); err != nil {
		t.Fatal(err)
	}
}

func stateFile(pid int, controlURL string) string {
	s := fmt.Sprintf("---\npid: %d\nrunning_from: /srv/app\n", pid)
	if controlURL != "" {
		s += fmt.Sprintf("control_url: %s\ncontrol_auth_token: t0ken\n", controlURL)
	}
	return s
}

// controlApp serves a minimal cluster /stats and returns its tcp:// control URL.
func controlApp(t *testing.T) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("token") != "t0ken" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		fmt.Fprint(w, `{"workers":2,"booted_workers":2,"worker_status":[{"pid":11,"index":0,"booted":true},{"pid":12,"index":1,"booted":true}]}`)
	}))
	t.Cleanup(srv.Close)
	return strings.Replace(srv.URL, "http://", "tcp://", 1)
}

func TestIndexStates(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "myapp-production/shared/tmp/pids/puma.state"), stateFile(100, "tcp://127.0.0.1:9293"), 0o644)
	writeFile(t, filepath.Join(root, "myapp-staging/shared/tmp/pids/puma.state"), stateFile(200, ""), 0o644)
	writeFile(t, filepath.Join(root, "broken/shared/tmp/pids/puma.state"), "---\nrunning: true\n", 0o644)
	writeFile(t, filepath.Join(root, "secret/shared/tmp/pids/puma.state"), stateFile(300, "tcp://127.0.0.1:9295"), 0o000)

	globs := []string{
		filepath.Join(root, "*/shared/tmp/pids/puma.state"),
		filepath.Join(root, "myapp-*/shared/tmp/pids/puma.state"), // overlapping: read once
		filepath.Join(root, "missing/*.state"),
	}
	index, issues := indexStates(globs)

	if len(index) != 2 || index[100].noControl || !index[200].noControl {
		t.Errorf("index = %+v, want pid 100 with control app, pid 200 without", index)
	}
	if got := index[100].state.ControlURL; got != "tcp://127.0.0.1:9293" {
		t.Errorf("pid 100 control_url = %q", got)
	}

	want := []string{"broken/shared/tmp/pids/puma.state: no pid"}
	if os.Geteuid() != 0 { // root reads a 0000 file
		want = append(want, "secret/shared/tmp/pids/puma.state: permission denied")
	}
	if len(issues) != len(want) {
		t.Fatalf("issues = %q, want %d issue(s)", issues, len(want))
	}
	for i, w := range want {
		parts := strings.SplitN(w, ": ", 2)
		if !strings.Contains(issues[i], parts[0]) || !strings.Contains(issues[i], parts[1]) {
			t.Errorf("issue %d = %q, want %q", i, issues[i], w)
		}
	}
}

func TestCollect(t *testing.T) {
	url := controlApp(t)
	procRoot := t.TempDir()

	// Master 500 has a readable working directory holding the default state file.
	appDir := t.TempDir()
	writeFile(t, filepath.Join(appDir, defaultStateFile), stateFile(500, url), 0o644)
	if err := os.MkdirAll(filepath.Join(procRoot, "500"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(appDir, filepath.Join(procRoot, "500", "cwd")); err != nil {
		t.Fatal(err)
	}

	instance := func(masterPIDs ...int) *puma.Instance {
		inst := &puma.Instance{Tag: "myapp_prod"}
		for _, pid := range masterPIDs {
			inst.Masters = append(inst.Masters, puma.Process{PID: pid})
		}
		return inst
	}
	states := map[int]stateEntry{
		100: {state: puma.State{PID: 100, ControlURL: url, ControlAuthToken: "t0ken"}},
		200: {state: puma.State{PID: 200}, noControl: true},
	}

	tests := []struct {
		name        string
		inst        *puma.Instance
		globs       bool
		wantStats   bool
		wantNote    string
		wantNoState bool
	}{
		{name: "matched by master pid", inst: instance(100), globs: true, wantStats: true},
		{name: "second master matched", inst: instance(999, 100), globs: true, wantStats: true},
		{name: "matched, no control app", inst: instance(200), globs: true, wantNote: "no control app"},
		{name: "no match, cwd fallback", inst: instance(500), globs: true, wantStats: true},
		{name: "no match, cwd unreadable", inst: instance(600), globs: true, wantNote: "no state file", wantNoState: true},
		{name: "no glob, cwd unreadable", inst: instance(600), wantNote: "master cwd unreadable, set --state-glob", wantNoState: true},
		{name: "orphan workers only", inst: instance()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := collector{procRoot: procRoot, states: states, globs: tt.globs, timeout: 5 * time.Second}
			r := c.collect(tt.inst)
			if r.StatsErr != nil {
				t.Fatalf("StatsErr = %v", r.StatsErr)
			}
			if (r.Stats != nil) != tt.wantStats || r.Note != tt.wantNote || r.NoState != tt.wantNoState {
				t.Errorf("collect() stats=%v note=%q noState=%v, want stats=%v note=%q noState=%v",
					r.Stats != nil, r.Note, r.NoState, tt.wantStats, tt.wantNote, tt.wantNoState)
			}
			if tt.wantStats && r.Stats.BootedWorkers != 2 {
				t.Errorf("stats = %+v", r.Stats)
			}
		})
	}
}

func TestEvaluateStateIssues(t *testing.T) {
	issue := "/srv/x/puma.state: permission denied"

	// The unreadable file may be the state file of an instance without one: warning.
	noState := healthy("myapp_prod")
	noState.Stats, noState.StatePID, noState.Note, noState.NoState = nil, 0, "no state file", true
	got := evaluate(nil, []Report{noState}, th, now, issue)
	want := "1 instance(s) OK, 1 state file issue(s)\n" +
		"myapp_prod OK: 2 worker(s), no stats (no state file), rss max 300MB\n" +
		"state file WARNING: " + issue
	if got.State != sensu.CheckStateWarning || got.Message != want {
		t.Errorf("evaluate() = %d %q\nwant 1 %q", got.State, got.Message, want)
	}

	// Every checked instance has its state file: the unreadable one belongs to another app.
	got = evaluate(nil, []Report{healthy("myapp_prod")}, th, now, issue)
	if got.State != sensu.CheckStateOK || strings.Contains(got.Message, "state file") {
		t.Errorf("evaluate() = %d %q, want OK without state file issue", got.State, got.Message)
	}

	// Same when the instance without state file is ignored.
	got = evaluate([]string{"myapp_staging"}, []Report{noState, healthy("myapp_staging")}, th, now, issue)
	if got.State != sensu.CheckStateOK || strings.Contains(got.Message, "state file") {
		t.Errorf("evaluate() = %d %q, want OK without state file issue", got.State, got.Message)
	}
}

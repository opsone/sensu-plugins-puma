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
		if issues[i].Source != "state file" || !strings.Contains(issues[i].Text, parts[0]) || !strings.Contains(issues[i].Text, parts[1]) {
			t.Errorf("issue %d = %q, want %q", i, issues[i], w)
		}
	}
}

func TestCollect(t *testing.T) {
	url := controlApp(t)
	instance := func(masterPIDs ...int) *Instance {
		inst := &Instance{Tag: "myapp_prod"}
		for _, pid := range masterPIDs {
			inst.Masters = append(inst.Masters, Process{PID: pid})
		}
		return inst
	}
	states := map[int]stateEntry{
		100: {state: State{PID: 100, ControlURL: url, ControlAuthToken: "t0ken"}},
		200: {state: State{PID: 200}, noControl: true},
	}

	tests := []struct {
		name        string
		inst        *Instance
		wantStats   bool
		wantNote    string
		wantNoState bool
	}{
		{name: "matched by master pid", inst: instance(100), wantStats: true},
		{name: "second master matched", inst: instance(999, 100), wantStats: true},
		{name: "matched, no control app", inst: instance(200), wantNote: "no control app"},
		{name: "no state file", inst: instance(600), wantNote: "no state file", wantNoState: true},
		{name: "orphan workers only", inst: instance()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &collector{states: states, globs: true, timeout: 5 * time.Second}
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
	issue := Issue{"state file", "/srv/x/puma.state: permission denied"}

	// The unreadable file may be the state file of an instance without one: warning.
	noState := healthy("myapp_prod")
	noState.Stats, noState.StatePID, noState.Note, noState.NoState = nil, 0, "no state file", true
	got := evaluate(nil, []Report{noState}, th, now, nil, issue)
	want := "1 instance(s) OK, 1 stats source issue(s)\n" +
		"myapp_prod OK: 2 worker(s), no stats (no state file), rss max 300MB\n" +
		"state file WARNING: " + issue.Text
	if got.State != sensu.CheckStateWarning || got.Message != want {
		t.Errorf("evaluate() = %d %q\nwant 1 %q", got.State, got.Message, want)
	}

	// Every checked instance has its state file: the unreadable one belongs to another app.
	got = evaluate(nil, []Report{healthy("myapp_prod")}, th, now, nil, issue)
	if got.State != sensu.CheckStateOK || strings.Contains(got.Message, "state file") {
		t.Errorf("evaluate() = %d %q, want OK without state file issue", got.State, got.Message)
	}

	// Same when the instance without state file is ignored.
	got = evaluate([]string{"myapp_staging"}, []Report{noState, healthy("myapp_staging")}, th, now, nil, issue)
	if got.State != sensu.CheckStateOK || strings.Contains(got.Message, "state file") {
		t.Errorf("evaluate() = %d %q, want OK without state file issue", got.State, got.Message)
	}
}

// controlNoToken serves /stats without token (no_token) for the given worker PIDs.
func controlNoToken(t *testing.T, pids ...int) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" {
			http.Error(w, "unexpected query", http.StatusBadRequest)
			return
		}
		var ws []string
		for i, pid := range pids {
			ws = append(ws, fmt.Sprintf(`{"pid":%d,"index":%d,"booted":true}`, pid, i))
		}
		fmt.Fprintf(w, `{"workers":%d,"booted_workers":%d,"worker_status":[%s]}`, len(pids), len(pids), strings.Join(ws, ","))
	}))
	t.Cleanup(srv.Close)
	return strings.Replace(srv.URL, "http://", "tcp://", 1)
}

// closedPort returns a tcp:// URL nothing listens on.
func closedPort(t *testing.T) string {
	srv := httptest.NewServer(http.NotFoundHandler())
	u := strings.Replace(srv.URL, "http://", "tcp://", 1)
	srv.Close()
	return u
}

func TestCollectControl(t *testing.T) {
	prod, staging, down := controlNoToken(t, 11, 12), controlNoToken(t, 21, 22), closedPort(t)
	instance := func(master int, workers ...int) *Instance {
		inst := &Instance{Tag: "myapp_prod", Masters: []Process{{PID: master}}}
		for _, pid := range workers {
			inst.Workers = append(inst.Workers, Worker{Process: Process{PID: pid}, MasterPID: master})
		}
		return inst
	}
	states := map[int]stateEntry{
		100: {state: State{PID: 100, ControlURL: controlApp(t), ControlAuthToken: "t0ken"}},
		200: {state: State{PID: 200}, noControl: true},
	}

	tests := []struct {
		name        string
		globs       bool
		inst        *Instance
		wantWorkers int // first worker PID of the stats, 0 without stats
		wantNote    string
		wantNoState bool
	}{
		{name: "matched by worker pid", inst: instance(1, 21, 22), wantWorkers: 21},
		{name: "one worker in common (phased restart)", inst: instance(1, 12, 99), wantWorkers: 11},
		{name: "state file first", globs: true, inst: instance(100, 21), wantWorkers: 11},
		{name: "state file without control app, control matched", globs: true, inst: instance(200, 11), wantWorkers: 11},
		{name: "state file without control app, no match", globs: true, inst: instance(200, 31), wantNote: "no control app"},
		{name: "no match", inst: instance(1, 31), wantNote: "no control app matched", wantNoState: true},
		{name: "no match, no state file", globs: true, inst: instance(1, 31), wantNote: "no state file, no control app matched", wantNoState: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &collector{states: states, globs: tt.globs, controls: []string{prod, staging, down}, timeout: 5 * time.Second}
			r := c.collect(tt.inst)
			if r.StatsErr != nil {
				t.Fatalf("StatsErr = %v", r.StatsErr)
			}
			got := 0
			if r.Stats != nil {
				got = r.Stats.WorkerStatus[0].PID
			}
			if got != tt.wantWorkers || r.Note != tt.wantNote || r.NoState != tt.wantNoState {
				t.Errorf("collect() worker=%d note=%q noState=%v, want worker=%d note=%q noState=%v",
					got, r.Note, r.NoState, tt.wantWorkers, tt.wantNote, tt.wantNoState)
			}
			if c.polled && (len(c.issues) != 1 || c.issues[0].Source != "control app" || !strings.HasPrefix(c.issues[0].Text, down[:len(down)-1])) {
				t.Errorf("issues = %+v, want %s unreachable", c.issues, down)
			}
		})
	}

	// Every instance has its state file: the --control endpoints are not queried.
	c := &collector{states: states, globs: true, controls: []string{down}, timeout: 5 * time.Second}
	if r := c.collect(instance(100, 11)); r.Stats == nil || c.polled || len(c.issues) != 0 {
		t.Errorf("collect() stats=%v polled=%v issues=%v, want stats without polling", r.Stats != nil, c.polled, c.issues)
	}
}

func TestEvaluateControlIssues(t *testing.T) {
	issue := Issue{"control app", "tcp://127.0.0.1:9294: connection refused"}
	noStats := healthy("myapp_prod")
	noStats.Stats, noStats.StatePID, noStats.Note, noStats.NoState = nil, 0, "no control app matched", true
	got := evaluate(nil, []Report{noStats}, th, now, nil, issue)
	if got.State != sensu.CheckStateWarning || !strings.Contains(got.Message, "\ncontrol app WARNING: tcp://127.0.0.1:9294: connection refused") {
		t.Errorf("evaluate() = %d %q, want WARNING with the unreachable control app", got.State, got.Message)
	}
	got = evaluate(nil, []Report{healthy("myapp_prod")}, th, now, nil, issue)
	if got.State != sensu.CheckStateOK || strings.Contains(got.Message, "control app") {
		t.Errorf("evaluate() = %d %q, want OK: every instance has its stats", got.State, got.Message)
	}
}

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sensu/sensu-plugin-sdk/sensu"
)

var graceTh = func() Thresholds {
	t := th
	t.RestartGrace, t.LoadGrace = 5*time.Minute, 3*time.Minute
	return t
}()

// tracker returns an in-memory tracker: conditions first seen ago.
func tracker(ago map[string]time.Duration) *Tracker {
	t := &Tracker{now: now, seen: map[string]int64{}, next: map[string]int64{}}
	for k, d := range ago {
		t.seen[k] = now.Add(-d).Unix()
	}
	return t
}

func TestEvaluateGrace(t *testing.T) {
	busy := func(r *Report) {
		r.Stats.WorkerStatus = []WorkerStatus{workerStatus(101, 0, 5, 0), workerStatus(102, 1, 4, 0)}
	}
	saturated := func(r *Report) {
		r.Stats.WorkerStatus = []WorkerStatus{workerStatus(101, 0, 5, 0), workerStatus(102, 1, 0, 0)}
	}
	backlog := func(r *Report) { r.Stats.WorkerStatus[0].LastStatus.Backlog = 7 }
	noMaster := func(r *Report) { r.Instance.Masters = nil }
	booting := func(r *Report) { r.Stats.BootedWorkers = 0 }
	allYoung := func(r *Report) {
		for i := range r.Instance.Workers {
			r.Instance.Workers[i].StartedAt = now.Add(-10 * time.Second)
		}
	}
	oneYoung := func(r *Report) { r.Instance.Workers[1].StartedAt = now.Add(-10 * time.Second) }

	tests := []struct {
		name    string
		mutate  func(r *Report)
		absent  bool
		ago     map[string]time.Duration
		noState bool
		state   int
		want    string
	}{
		{name: "busy, new", mutate: busy, state: sensu.CheckStateOK, want: "threads 9/10 busy (90%) for 0s, within grace"},
		{name: "busy past grace", mutate: busy, ago: map[string]time.Duration{"load:myapp_prod:busy": 3 * time.Minute}, state: sensu.CheckStateWarning, want: "myapp_prod WARNING: threads 9/10 busy (90%) for 3m0s"},
		{name: "busy, no state", mutate: busy, noState: true, state: sensu.CheckStateWarning, want: "myapp_prod WARNING: threads 9/10 busy (90%)\n"},
		{name: "saturated worker within grace", mutate: saturated, ago: map[string]time.Duration{"load:myapp_prod:worker0": 2 * time.Minute}, state: sensu.CheckStateOK, want: "worker 0 threads 5/5 busy for 2m0s, within grace"},
		{name: "saturated worker past grace", mutate: saturated, ago: map[string]time.Duration{"load:myapp_prod:worker0": 4 * time.Minute}, state: sensu.CheckStateWarning, want: "worker 0 threads 5/5 busy for 4m0s"},
		{name: "backlog, new", mutate: backlog, state: sensu.CheckStateOK, want: "backlog 7 for 0s, within grace"},
		{name: "master not running, new", mutate: noMaster, state: sensu.CheckStateOK, want: "master not running for 0s, within grace"},
		{name: "master not running past grace", mutate: noMaster, ago: map[string]time.Duration{"restart:myapp_prod": 5 * time.Minute}, state: sensu.CheckStateCritical, want: "myapp_prod CRITICAL: master not running for 5m0s"},
		{name: "booting within grace", mutate: booting, ago: map[string]time.Duration{"restart:myapp_prod": time.Minute}, state: sensu.CheckStateOK, want: "0/2 worker(s) booted for 1m0s, within grace"},
		{name: "booting past grace", mutate: booting, ago: map[string]time.Duration{"restart:myapp_prod": 10 * time.Minute}, state: sensu.CheckStateCritical, want: "0/2 worker(s) booted for 10m0s"},
		{name: "not running, new", absent: true, state: sensu.CheckStateOK, want: "myapp_prod OK: not running for 0s, within grace"},
		{name: "not running past grace", absent: true, ago: map[string]time.Duration{"restart:myapp_prod": 6 * time.Minute}, state: sensu.CheckStateCritical, want: "myapp_prod CRITICAL: not running for 6m0s"},
		{name: "not running, no state", absent: true, noState: true, state: sensu.CheckStateCritical, want: "myapp_prod CRITICAL: not running\n"},
		{name: "hot restart: every worker young", mutate: allYoung, state: sensu.CheckStateOK, want: "myapp_prod OK"},
		{name: "one worker restarted", mutate: oneYoung, state: sensu.CheckStateWarning, want: "myapp_prod WARNING: worker 1 restarted 10s ago"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := healthy("myapp_prod")
			if tt.mutate != nil {
				tt.mutate(&r)
			}
			var reports []Report
			if !tt.absent {
				reports = []Report{r}
			}
			tr := tracker(tt.ago)
			if tt.noState {
				tr.err = errors.New("no state")
			}
			got := evaluate([]string{"myapp_prod"}, reports, graceTh, now, tr)
			if got.State != tt.state || !strings.Contains(got.Message+"\n", tt.want) {
				t.Errorf("evaluate() = %d %q, want %d with %q", got.State, got.Message, tt.state, tt.want)
			}
			if tt.noState && !strings.Contains(got.Message, "grace WARNING: no state (no grace)") {
				t.Errorf("state error not reported: %q", got.Message)
			}
		})
	}
}

func TestTracker(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, graceFile)

	// Nothing seen, no state: nothing written.
	tr := LoadTracker(dir, now)
	if err := tr.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state written without condition: %v", err)
	}

	// First seen: now; the next runs keep it.
	tr = LoadTracker(dir, now)
	if got := tr.Since("restart:a"); !got.Equal(now) {
		t.Errorf("Since = %v, want %v", got, now)
	}
	if err := tr.Save(); err != nil {
		t.Fatal(err)
	}
	tr = LoadTracker(dir, now.Add(time.Minute))
	if got := tr.Since("restart:a"); !got.Equal(now) {
		t.Errorf("Since next run = %v, want %v", got, now)
	}
	tr.Save()

	// Condition gone for one run: dropped; seen again: new time.
	LoadTracker(dir, now.Add(2*time.Minute)).Save()
	tr = LoadTracker(dir, now.Add(3*time.Minute))
	if got := tr.Since("restart:a"); !got.Equal(now.Add(3 * time.Minute)) {
		t.Errorf("Since after recovery = %v", got)
	}

	// Corrupted state: start over.
	os.WriteFile(path, []byte("garbage"), 0o644)
	tr = LoadTracker(dir, now)
	if got := tr.Since("restart:a"); !got.Equal(now) || tr.Err() != nil {
		t.Errorf("corrupted state: Since %v, err %v", got, tr.Err())
	}

	var none *Tracker
	if !none.Since("x").IsZero() || none.Err() != nil || none.Save() != nil {
		t.Error("nil tracker")
	}
}

func TestTrackerUnwritable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing")
	if tr := LoadTracker(dir, now); tr.Save() != nil || tr.Err() != nil {
		t.Errorf("error without condition")
	}

	// Within grace at first, then evaluated again without grace once the state cannot be kept.
	r := healthy("myapp_prod")
	r.Instance.Masters = nil
	tr := LoadTracker(dir, now)
	if got := evaluate(nil, []Report{r}, graceTh, now, tr); got.State != sensu.CheckStateOK {
		t.Errorf("first evaluate() = %d %q", got.State, got.Message)
	}
	if tr.Save() == nil {
		t.Fatal("Save() succeeded in a missing directory")
	}
	got := evaluate(nil, []Report{r}, graceTh, now, tr)
	if got.State != sensu.CheckStateCritical ||
		!strings.Contains(got.Message, "myapp_prod CRITICAL: master not running") ||
		!strings.Contains(got.Message, "grace WARNING: writing "+filepath.Join(dir, graceFile)) {
		t.Errorf("evaluate() = %d %q", got.State, got.Message)
	}
}

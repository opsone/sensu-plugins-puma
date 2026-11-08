package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/opsone/sensu-plugins-puma/internal/puma"
	"github.com/sensu/sensu-plugin-sdk/sensu"
)

var (
	now = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	th  = Thresholds{
		MinWorkers: 2, BusyWarn: 80, BusyCrit: 95, BacklogWarn: 5, BacklogCrit: 20,
		CheckinMax: time.Minute, PhaseMax: 15 * time.Minute, WorkerMinAge: time.Minute,
		RSSWarn: 800 << 20, RSSCrit: 1000 << 20,
	}
)

func process(pid int, age time.Duration) puma.Process {
	return puma.Process{PID: pid, StartedAt: now.Add(-age), RSS: 300 << 20}
}

// healthy returns a 2-worker instance, up for one hour, with its control app stats.
func healthy(tag string) Report {
	app, env := puma.SplitTag(tag)
	return Report{
		Instance: &puma.Instance{
			Tag: tag, App: app, Env: env,
			Masters: []puma.Process{process(100, time.Hour)},
			Workers: []puma.Worker{
				{Process: process(101, time.Hour), Index: 0, MasterPID: 100},
				{Process: process(102, time.Hour), Index: 1, MasterPID: 100},
			},
		},
		StatePID: 100,
		Stats: &puma.Stats{
			Workers: 2, BootedWorkers: 2,
			WorkerStatus: []puma.WorkerStatus{
				workerStatus(101, 0, 1, 0),
				workerStatus(102, 1, 0, 0),
			},
		},
	}
}

func workerStatus(pid, index, busy, backlog int) puma.WorkerStatus {
	return puma.WorkerStatus{
		StartedAt: now.Add(-time.Hour), PID: pid, Index: index, Booted: true, LastCheckin: now.Add(-5 * time.Second),
		LastStatus: puma.ThreadStatus{Backlog: backlog, Running: 5, PoolCapacity: 5 - busy, MaxThreads: 5},
	}
}

func TestEvaluateHealthy(t *testing.T) {
	got := evaluate([]string{"myapp_prod"}, []Report{healthy("myapp_prod")}, th, now)
	want := "1 instance(s) OK\nmyapp_prod OK: 2 worker(s), 2/2 booted, threads 1/10 busy, backlog 0, rss max 300MB"
	if got.State != sensu.CheckStateOK || got.Message != want {
		t.Errorf("evaluate() = %d %q, want 0 %q", got.State, got.Message, want)
	}
}

func TestEvaluateNothing(t *testing.T) {
	got := evaluate(nil, nil, th, now)
	if got.State != sensu.CheckStateOK || got.Message != "no puma instance found" {
		t.Errorf("evaluate() = %+v", got)
	}
}

func TestEvaluate(t *testing.T) {
	tests := []struct {
		name     string
		expected []string
		mutate   func(r *Report)
		state    int
		contains string
	}{
		{
			name:     "expected instance missing",
			expected: []string{"myapp_prod", "myapp_staging"},
			state:    sensu.CheckStateCritical,
			contains: "1/2 instance(s) in trouble\nmyapp_prod OK: 2 worker(s)",
		},
		{
			name:     "missing staging line",
			expected: []string{"myapp_staging", "myapp_prod"},
			state:    sensu.CheckStateCritical,
			contains: "myapp_staging CRITICAL: not running",
		},
		{
			name:     "unexpected instance is flagged, not alerted",
			expected: []string{"other_prod"},
			state:    sensu.CheckStateCritical,
			contains: "myapp_prod OK: 2 worker(s), 2/2 booted, threads 1/10 busy, backlog 0, rss max 300MB (not expected)",
		},
		{
			name: "master gone, orphan workers",
			mutate: func(r *Report) {
				r.Instance.Masters = nil
				r.Stats, r.StatePID = nil, 0
			},
			state:    sensu.CheckStateCritical,
			contains: "myapp_prod CRITICAL: master not running; 2 orphan worker(s)",
		},
		{
			name:     "one worker process left",
			mutate:   func(r *Report) { r.Instance.Workers = r.Instance.Workers[:1] },
			state:    sensu.CheckStateWarning,
			contains: "1/2 worker process(es)",
		},
		{
			name:     "no worker process",
			mutate:   func(r *Report) { r.Instance.Workers = nil },
			state:    sensu.CheckStateCritical,
			contains: "0/2 worker process(es)",
		},
		{
			name:     "two masters with the same tag",
			mutate:   func(r *Report) { r.Instance.Masters = append(r.Instance.Masters, process(200, time.Minute)) },
			state:    sensu.CheckStateWarning,
			contains: "2 masters running (pid 100,200)",
		},
		{
			name:     "worker restarted recently",
			mutate:   func(r *Report) { r.Instance.Workers[1].StartedAt = now.Add(-12 * time.Second) },
			state:    sensu.CheckStateWarning,
			contains: "worker 1 restarted 12s ago",
		},
		{
			name: "fresh start is not a restart",
			mutate: func(r *Report) {
				r.Instance.Masters[0].StartedAt = now.Add(-20 * time.Second)
				r.Instance.Workers[1].StartedAt = now.Add(-12 * time.Second)
			},
			state: sensu.CheckStateOK,
		},
		{
			name:     "worker rss critical",
			mutate:   func(r *Report) { r.Instance.Workers[0].RSS = 1200 << 20 },
			state:    sensu.CheckStateCritical,
			contains: "worker 0 rss 1200MB",
		},
		{
			name:     "worker rss warning",
			mutate:   func(r *Report) { r.Instance.Workers[0].RSS = 900 << 20 },
			state:    sensu.CheckStateWarning,
			contains: "worker 0 rss 900MB",
		},
		{
			name:     "no state file: informational",
			mutate:   func(r *Report) { r.Stats, r.StatePID, r.Note = nil, 0, "no tmp/pids/puma.state" },
			state:    sensu.CheckStateOK,
			contains: "myapp_prod OK: 2 worker(s), no stats (no tmp/pids/puma.state), rss max 300MB",
		},
		{
			name:     "control app unreachable",
			mutate:   func(r *Report) { r.Stats, r.StatsErr = nil, errors.New("connection refused") },
			state:    sensu.CheckStateWarning,
			contains: "stats unavailable: connection refused",
		},
		{
			name:     "stale state file",
			mutate:   func(r *Report) { r.StatePID = 99 },
			state:    sensu.CheckStateWarning,
			contains: "state file pid 99 is not a running master",
		},
		{
			name:     "single mode",
			mutate:   func(r *Report) { r.Stats = &puma.Stats{} },
			state:    sensu.CheckStateWarning,
			contains: "not running in cluster mode",
		},
		{
			name:     "worker not booted",
			mutate:   func(r *Report) { r.Stats.BootedWorkers = 1; r.Stats.WorkerStatus[1].Booted = false },
			state:    sensu.CheckStateWarning,
			contains: "1/2 worker(s) booted",
		},
		{
			name: "no worker booted",
			mutate: func(r *Report) {
				r.Stats.BootedWorkers = 0
				r.Stats.WorkerStatus[0].Booted, r.Stats.WorkerStatus[1].Booted = false, false
			},
			state:    sensu.CheckStateCritical,
			contains: "0/2 worker(s) booted",
		},
		{
			name:     "worker hung",
			mutate:   func(r *Report) { r.Stats.WorkerStatus[0].LastCheckin = now.Add(-95 * time.Second) },
			state:    sensu.CheckStateCritical,
			contains: "worker 0 no checkin for 1m35s",
		},
		{
			name: "phased restart stuck",
			mutate: func(r *Report) {
				r.Stats.Phase, r.Stats.OldWorkers = 1, 1
				r.Stats.WorkerStatus[0].Phase = 1
				r.Stats.WorkerStatus[0].StartedAt = now.Add(-20 * time.Minute)
			},
			state:    sensu.CheckStateWarning,
			contains: "phased restart stuck: 1 old worker(s) for 20m0s",
		},
		{
			name: "phased restart in progress, young worker not reported",
			mutate: func(r *Report) {
				r.Stats.Phase, r.Stats.OldWorkers = 1, 1
				r.Stats.WorkerStatus[0].Phase = 1
				r.Stats.WorkerStatus[0].StartedAt = now.Add(-10 * time.Second)
				r.Instance.Workers[0].StartedAt = now.Add(-10 * time.Second)
			},
			state: sensu.CheckStateOK,
		},
		{
			name: "threads busy warning",
			mutate: func(r *Report) {
				r.Stats.WorkerStatus[0] = workerStatus(101, 0, 4, 0)
				r.Stats.WorkerStatus[1] = workerStatus(102, 1, 4, 0)
			},
			state:    sensu.CheckStateWarning,
			contains: "threads 8/10 busy (80%)",
		},
		{
			name: "threads busy critical",
			mutate: func(r *Report) {
				r.Stats.WorkerStatus[0] = workerStatus(101, 0, 5, 0)
				r.Stats.WorkerStatus[1] = workerStatus(102, 1, 5, 0)
			},
			state:    sensu.CheckStateCritical,
			contains: "threads 10/10 busy (100%)",
		},
		{
			name:     "single saturated worker",
			mutate:   func(r *Report) { r.Stats.WorkerStatus[1] = workerStatus(102, 1, 5, 0) },
			state:    sensu.CheckStateWarning,
			contains: "worker 1 threads 5/5 busy",
		},
		{
			name: "backlog critical",
			mutate: func(r *Report) {
				r.Stats.WorkerStatus[0] = workerStatus(101, 0, 1, 12)
				r.Stats.WorkerStatus[1] = workerStatus(102, 1, 1, 10)
			},
			state:    sensu.CheckStateCritical,
			contains: "backlog 22",
		},
		{
			name: "backlog of a non booted worker is ignored",
			mutate: func(r *Report) {
				r.Stats.WorkerStatus[1].Booted = false
				r.Stats.WorkerStatus[1].LastStatus.Backlog = 50
			},
			state:    sensu.CheckStateOK,
			contains: "backlog 0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := healthy("myapp_prod")
			if tt.mutate != nil {
				tt.mutate(&r)
			}
			got := evaluate(tt.expected, []Report{r}, th, now)
			if got.State != tt.state || !strings.Contains(got.Message, tt.contains) {
				t.Errorf("evaluate() = %d %q\nwant %d containing %q", got.State, got.Message, tt.state, tt.contains)
			}
		})
	}
}

func TestExpectedTags(t *testing.T) {
	got := expectedTags([]string{"myapp_prod, myapp_staging", "", "other_prod"})
	want := []string{"myapp_prod", "myapp_staging", "other_prod"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("expectedTags() = %v, want %v", got, want)
	}
	if got := expectedTags([]string{""}); got != nil {
		t.Errorf("expectedTags(empty token) = %v, want nil", got)
	}
}

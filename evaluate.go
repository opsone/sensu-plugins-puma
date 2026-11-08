package main

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/opsone/sensu-plugins-puma/internal/puma"
	"github.com/sensu/sensu-plugin-sdk/sensu"
)

// Thresholds of the check. A zero value disables the corresponding test.
type Thresholds struct {
	MinWorkers   int
	BusyWarn     int // % of busy threads
	BusyCrit     int
	BacklogWarn  int
	BacklogCrit  int
	CheckinMax   time.Duration
	PhaseMax     time.Duration
	WorkerMinAge time.Duration
	RSSWarn      uint64 // bytes, per worker
	RSSCrit      uint64
}

// Report gathers what is known about one instance before evaluation.
type Report struct {
	Instance *puma.Instance
	StatePID int         // PID found in the state file, 0 if not read
	Stats    *puma.Stats // nil when the control app was not queried or failed
	StatsErr error       // control app expected but unusable
	Note     string      // why stats are missing, informational
}

// Result is the outcome of the evaluation.
type Result struct {
	State   int
	Message string
}

type finding struct {
	state int
	text  string
}

type instanceResult struct {
	state int
	line  string
}

// evaluate turns the discovered instances into a check result: one summary line, then one line per instance.
func evaluate(expected []string, reports []Report, th Thresholds, now time.Time) Result {
	byTag := map[string]Report{}
	for _, r := range reports {
		byTag[r.Instance.Tag] = r
	}
	tags := make([]string, 0, len(byTag)+len(expected))
	for tag := range byTag {
		tags = append(tags, tag)
	}
	for _, tag := range expected {
		if _, ok := byTag[tag]; !ok {
			tags = append(tags, tag)
		}
	}
	slices.Sort(tags)
	tags = slices.Compact(tags)

	if len(tags) == 0 {
		return Result{sensu.CheckStateOK, "no puma instance found"}
	}

	worst, inTrouble := sensu.CheckStateOK, 0
	lines := make([]string, 0, len(tags))
	for _, tag := range tags {
		var res instanceResult
		r, found := byTag[tag]
		if !found {
			res = instanceResult{sensu.CheckStateCritical, fmt.Sprintf("%s CRITICAL: not running", tag)}
		} else {
			res = evaluateInstance(r, th, now)
			if len(expected) > 0 && !slices.Contains(expected, tag) {
				res.line += " (not expected)"
			}
		}
		worst = max(worst, res.state)
		if res.state != sensu.CheckStateOK {
			inTrouble++
		}
		lines = append(lines, res.line)
	}

	summary := fmt.Sprintf("%d instance(s) OK", len(tags))
	if inTrouble > 0 {
		summary = fmt.Sprintf("%d/%d instance(s) in trouble", inTrouble, len(tags))
	}
	return Result{worst, summary + "\n" + strings.Join(lines, "\n")}
}

func evaluateInstance(r Report, th Thresholds, now time.Time) instanceResult {
	inst := r.Instance
	var findings []finding
	add := func(state int, format string, args ...any) {
		findings = append(findings, finding{state, fmt.Sprintf(format, args...)})
	}

	// Processes.
	masters := map[int]puma.Process{}
	for _, m := range inst.Masters {
		masters[m.PID] = m
	}
	var workers, orphans []puma.Worker
	for _, w := range inst.Workers {
		if _, ok := masters[w.MasterPID]; ok {
			workers = append(workers, w)
		} else {
			orphans = append(orphans, w)
		}
	}

	switch len(inst.Masters) {
	case 0:
		add(sensu.CheckStateCritical, "master not running")
	case 1:
	default:
		add(sensu.CheckStateWarning, "%d masters running (pid %s)", len(inst.Masters), pids(inst.Masters))
	}
	if len(orphans) > 0 {
		add(sensu.CheckStateWarning, "%d orphan worker(s)", len(orphans))
	}
	if len(inst.Masters) > 0 && th.MinWorkers > 0 && len(workers) < th.MinWorkers*len(inst.Masters) {
		state := sensu.CheckStateWarning
		if len(workers) == 0 {
			state = sensu.CheckStateCritical
		}
		add(state, "%d/%d worker process(es)", len(workers), th.MinWorkers)
	}

	// Recently restarted workers, while the master is not young itself (fresh start or deploy).
	// Skipped during a phased restart, where young workers are expected.
	phasedRestart := r.Stats != nil && r.Stats.OldWorkers > 0
	if th.WorkerMinAge > 0 && !phasedRestart {
		for _, w := range workers {
			master := masters[w.MasterPID]
			if age := now.Sub(w.StartedAt); age < th.WorkerMinAge && now.Sub(master.StartedAt) >= th.WorkerMinAge {
				add(sensu.CheckStateWarning, "worker %d restarted %s ago", w.Index, age.Round(time.Second))
			}
		}
	}

	var maxRSS uint64
	for _, w := range workers {
		maxRSS = max(maxRSS, w.RSS)
		switch {
		case th.RSSCrit > 0 && w.RSS >= th.RSSCrit:
			add(sensu.CheckStateCritical, "worker %d rss %s", w.Index, mb(w.RSS))
		case th.RSSWarn > 0 && w.RSS >= th.RSSWarn:
			add(sensu.CheckStateWarning, "worker %d rss %s", w.Index, mb(w.RSS))
		}
	}

	// Control app.
	summary := []string{fmt.Sprintf("%d worker(s)", len(workers))}
	switch {
	case r.StatsErr != nil:
		add(sensu.CheckStateWarning, "stats unavailable: %v", r.StatsErr)
	case r.Stats == nil:
		if r.Note != "" {
			summary = append(summary, "no stats ("+r.Note+")")
		}
	default:
		if r.StatePID != 0 && len(inst.Masters) > 0 {
			if _, ok := masters[r.StatePID]; !ok {
				add(sensu.CheckStateWarning, "state file pid %d is not a running master", r.StatePID)
			}
		}
		summary = append(summary, evaluateStats(r.Stats, workers, th, now, add)...)
	}
	if maxRSS > 0 {
		summary = append(summary, "rss max "+mb(maxRSS))
	}

	state := sensu.CheckStateOK
	texts := make([]string, 0, len(findings))
	for _, f := range findings {
		state = max(state, f.state)
		texts = append(texts, f.text)
	}
	if state == sensu.CheckStateOK {
		return instanceResult{state, fmt.Sprintf("%s OK: %s", inst.Tag, strings.Join(summary, ", "))}
	}
	return instanceResult{state, fmt.Sprintf("%s %s: %s", inst.Tag, stateLabel(state), strings.Join(texts, "; "))}
}

// evaluateStats checks the control app answer and returns the summary items of a healthy instance.
func evaluateStats(s *puma.Stats, workers []puma.Worker, th Thresholds, now time.Time, add func(int, string, ...any)) []string {
	if len(s.WorkerStatus) == 0 {
		add(sensu.CheckStateWarning, "not running in cluster mode")
		return nil
	}

	if s.BootedWorkers < s.Workers {
		state := sensu.CheckStateWarning
		if s.BootedWorkers == 0 {
			state = sensu.CheckStateCritical
		}
		add(state, "%d/%d worker(s) booted", s.BootedWorkers, s.Workers)
	}

	startedAt := map[int]time.Time{}
	for _, w := range workers {
		startedAt[w.PID] = w.StartedAt
	}

	var busy, capacity, backlog int
	var oldPhase int
	var phaseStart time.Time
	for _, w := range s.WorkerStatus {
		if w.Phase != s.Phase {
			oldPhase++
		} else {
			start := w.StartedAt
			if start.IsZero() {
				start = startedAt[w.PID]
			}
			if !start.IsZero() && (phaseStart.IsZero() || start.Before(phaseStart)) {
				phaseStart = start
			}
		}
		if !w.Booted {
			continue
		}
		if th.CheckinMax > 0 && !w.LastCheckin.IsZero() {
			if since := now.Sub(w.LastCheckin); since > th.CheckinMax {
				add(sensu.CheckStateCritical, "worker %d no checkin for %s", w.Index, since.Round(time.Second))
			}
		}
		ts := w.LastStatus
		busy += ts.Busy()
		capacity += ts.MaxThreads
		backlog += ts.Backlog
	}

	if oldPhase > 0 && th.PhaseMax > 0 && !phaseStart.IsZero() {
		if since := now.Sub(phaseStart); since > th.PhaseMax {
			add(sensu.CheckStateWarning, "phased restart stuck: %d old worker(s) for %s", oldPhase, since.Round(time.Second))
		}
	}

	globalAlert := false
	if capacity > 0 {
		ratio := busy * 100 / capacity
		switch {
		case th.BusyCrit > 0 && ratio >= th.BusyCrit:
			add(sensu.CheckStateCritical, "threads %d/%d busy (%d%%)", busy, capacity, ratio)
			globalAlert = true
		case th.BusyWarn > 0 && ratio >= th.BusyWarn:
			add(sensu.CheckStateWarning, "threads %d/%d busy (%d%%)", busy, capacity, ratio)
			globalAlert = true
		}
	}
	// A single saturated worker hidden by an acceptable average (slow requests piling up on it).
	if !globalAlert && th.BusyCrit > 0 {
		for _, w := range s.WorkerStatus {
			ts := w.LastStatus
			if w.Booted && ts.MaxThreads > 0 && ts.Busy()*100/ts.MaxThreads >= th.BusyCrit {
				add(sensu.CheckStateWarning, "worker %d threads %d/%d busy", w.Index, ts.Busy(), ts.MaxThreads)
			}
		}
	}

	switch {
	case th.BacklogCrit > 0 && backlog >= th.BacklogCrit:
		add(sensu.CheckStateCritical, "backlog %d", backlog)
	case th.BacklogWarn > 0 && backlog >= th.BacklogWarn:
		add(sensu.CheckStateWarning, "backlog %d", backlog)
	}

	return []string{
		fmt.Sprintf("%d/%d booted", s.BootedWorkers, s.Workers),
		fmt.Sprintf("threads %d/%d busy", busy, capacity),
		fmt.Sprintf("backlog %d", backlog),
	}
}

func pids(ps []puma.Process) string {
	s := make([]string, len(ps))
	for i, p := range ps {
		s[i] = fmt.Sprint(p.PID)
	}
	return strings.Join(s, ",")
}

func mb(b uint64) string {
	return fmt.Sprintf("%dMB", b/(1024*1024))
}

func stateLabel(state int) string {
	switch state {
	case sensu.CheckStateOK:
		return "OK"
	case sensu.CheckStateWarning:
		return "WARNING"
	case sensu.CheckStateCritical:
		return "CRITICAL"
	default:
		return "UNKNOWN"
	}
}

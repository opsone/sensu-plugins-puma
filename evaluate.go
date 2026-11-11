package main

import (
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"
	"time"

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
	RestartGrace time.Duration // restart conditions (not running, missing or booting workers) reported after this delay
	LoadGrace    time.Duration // load conditions (busy threads, backlog) reported after this delay
}

// Report gathers what is known about one instance before evaluation.
type Report struct {
	Instance *Instance
	StatePID int    // PID found in the state file, 0 if not read
	Stats    *Stats // nil when the control app was not queried or failed
	StatsErr error  // control app expected but unusable
	Note     string // why stats are missing, informational
	NoState  bool   // no state file found for the instance
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

// conditions collects the findings of an instance. A condition under a grace delay is only a finding
// once it has lasted the delay, immediately when its start is unknown (no state); within the delay it
// is a note, shown on the OK line.
type conditions struct {
	findings []finding
	notes    []string
	tr       *Tracker
	now      time.Time
}

func (c *conditions) add(state int, format string, args ...any) {
	c.findings = append(c.findings, finding{state, fmt.Sprintf(format, args...)})
}

func (c *conditions) graced(grace time.Duration, key string, state int, text string) {
	var since time.Time
	if grace > 0 {
		since = c.tr.Since(key)
	}
	switch d := c.now.Sub(since).Round(time.Second); {
	case since.IsZero():
		c.add(state, "%s", text)
	case d < grace:
		c.notes = append(c.notes, fmt.Sprintf("%s for %s, within grace", text, d))
	default:
		c.add(state, "%s for %s", text, d)
	}
}

func restartKey(tag string) string    { return "restart:" + tag }
func loadKey(tag, what string) string { return "load:" + tag + ":" + what }

// evaluate turns the discovered instances into a check result: one summary line, then one line per
// instance. With expected tags (names or patterns), only the matching instances are checked, the
// others are left out; expected names without wildcard must be running.
// Issues (unreadable state files) are warnings, reported only when a checked instance has no state
// file: they may hide its stats.
func evaluate(expected []string, reports []Report, th Thresholds, now time.Time, tr *Tracker, issues ...string) Result {
	byTag := map[string]Report{}
	for _, r := range reports {
		if selected(expected, r.Instance.Tag) {
			byTag[r.Instance.Tag] = r
		}
	}
	tags := slices.Concat(slices.Collect(maps.Keys(byTag)), required(expected))
	slices.Sort(tags)
	tags = slices.Compact(tags)

	stateMissing := false
	for _, r := range byTag {
		stateMissing = stateMissing || r.NoState
	}
	if !stateMissing {
		issues = nil
	}

	if len(tags) == 0 && len(issues) == 0 {
		msg := "no puma instance found"
		if len(reports) > 0 {
			msg = "no puma instance checked"
		}
		return Result{sensu.CheckStateOK, msg}
	}

	worst, inTrouble := sensu.CheckStateOK, 0
	lines := make([]string, 0, len(tags)+len(issues)+1)
	for _, tag := range tags {
		var res instanceResult
		if r, found := byTag[tag]; found {
			res = evaluateInstance(r, th, now, tr)
		} else {
			res = notRunning(tag, th, now, tr)
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
	if len(issues) > 0 {
		worst = max(worst, sensu.CheckStateWarning)
		summary += fmt.Sprintf(", %d state file issue(s)", len(issues))
		for _, issue := range issues {
			lines = append(lines, "state file WARNING: "+issue)
		}
	}
	if err := tr.Err(); err != nil {
		worst = max(worst, sensu.CheckStateWarning)
		lines = append(lines, fmt.Sprintf("grace WARNING: %v (no grace)", err))
	}
	return Result{worst, summary + "\n" + strings.Join(lines, "\n")}
}

// selected reports whether an instance is checked: no --expect, or a matching name or pattern.
func selected(expected []string, tag string) bool {
	if len(expected) == 0 {
		return true
	}
	for _, e := range expected {
		if ok, _ := path.Match(e, tag); ok {
			return true
		}
	}
	return false
}

// required returns the expected names without wildcard: the instances that must be running.
func required(expected []string) []string {
	var names []string
	for _, e := range expected {
		if !strings.ContainsAny(e, `*?[\`) {
			names = append(names, e)
		}
	}
	return names
}

// notRunning reports an expected instance not running, past the restart grace delay.
func notRunning(tag string, th Thresholds, now time.Time, tr *Tracker) instanceResult {
	c := &conditions{tr: tr, now: now}
	c.graced(th.RestartGrace, restartKey(tag), sensu.CheckStateCritical, "not running")
	if len(c.findings) == 0 {
		return instanceResult{sensu.CheckStateOK, fmt.Sprintf("%s OK: %s", tag, strings.Join(c.notes, ", "))}
	}
	return instanceResult{sensu.CheckStateCritical, fmt.Sprintf("%s CRITICAL: %s", tag, c.findings[0].text)}
}

func evaluateInstance(r Report, th Thresholds, now time.Time, tr *Tracker) instanceResult {
	inst := r.Instance
	c := &conditions{tr: tr, now: now}
	add := c.add
	// A restart (deployment) stops the master or the workers, then boots them again: one episode.
	restart := func(state int, format string, args ...any) {
		c.graced(th.RestartGrace, restartKey(inst.Tag), state, fmt.Sprintf(format, args...))
	}

	// Processes.
	masters := map[int]Process{}
	for _, m := range inst.Masters {
		masters[m.PID] = m
	}
	var workers, orphans []Worker
	for _, w := range inst.Workers {
		if _, ok := masters[w.MasterPID]; ok {
			workers = append(workers, w)
		} else {
			orphans = append(orphans, w)
		}
	}

	switch len(inst.Masters) {
	case 0:
		restart(sensu.CheckStateCritical, "master not running")
	case 1:
	default:
		add(sensu.CheckStateWarning, "%d masters running (pid %s)", len(inst.Masters), pids(inst.Masters))
	}
	if len(orphans) > 0 {
		restart(sensu.CheckStateWarning, "%d orphan worker(s)", len(orphans))
	}
	if len(inst.Masters) > 0 && th.MinWorkers > 0 && len(workers) < th.MinWorkers*len(inst.Masters) {
		state := sensu.CheckStateWarning
		if len(workers) == 0 {
			state = sensu.CheckStateCritical
		}
		restart(state, "%d/%d worker process(es)", len(workers), th.MinWorkers)
	}

	// Recently restarted workers, while the master is not young itself (fresh start or deploy).
	// Skipped during a phased restart, where young workers are expected, and when every worker is
	// young: a hot restart (pumactl restart, SIGUSR2) respawns them all under the same master. A
	// crashed or killed worker is respawned alone.
	phasedRestart := r.Stats != nil && r.Stats.OldWorkers > 0
	if th.WorkerMinAge > 0 && !phasedRestart {
		var young []string
		for _, w := range workers {
			master := masters[w.MasterPID]
			if age := now.Sub(w.StartedAt); age < th.WorkerMinAge && now.Sub(master.StartedAt) >= th.WorkerMinAge {
				young = append(young, fmt.Sprintf("worker %d restarted %s ago", w.Index, age.Round(time.Second)))
			}
		}
		if len(young) < len(workers) {
			for _, y := range young {
				add(sensu.CheckStateWarning, "%s", y)
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
		restart(sensu.CheckStateWarning, "stats unavailable: %v", r.StatsErr) // control app restarting with the master
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
		summary = append(summary, evaluateStats(r.Stats, workers, th, c, restart, inst.Tag)...)
	}
	if maxRSS > 0 {
		summary = append(summary, "rss max "+mb(maxRSS))
	}
	summary = append(summary, c.notes...)

	state := sensu.CheckStateOK
	texts := make([]string, 0, len(c.findings))
	for _, f := range c.findings {
		state = max(state, f.state)
		texts = append(texts, f.text)
	}
	if state == sensu.CheckStateOK {
		return instanceResult{state, fmt.Sprintf("%s OK: %s", inst.Tag, strings.Join(summary, ", "))}
	}
	return instanceResult{state, fmt.Sprintf("%s %s: %s", inst.Tag, stateLabel(state), strings.Join(texts, "; "))}
}

// evaluateStats checks the control app answer and returns the summary items of a healthy instance.
func evaluateStats(s *Stats, workers []Worker, th Thresholds, c *conditions, restart func(int, string, ...any), tag string) []string {
	now, add := c.now, c.add
	// A load peak (slow requests, bots) lasts a minute: only a lasting load is reported.
	load := func(what string, state int, format string, args ...any) {
		c.graced(th.LoadGrace, loadKey(tag, what), state, fmt.Sprintf(format, args...))
	}
	if len(s.WorkerStatus) == 0 {
		add(sensu.CheckStateWarning, "not running in cluster mode")
		return nil
	}

	if s.BootedWorkers < s.Workers {
		state := sensu.CheckStateWarning
		if s.BootedWorkers == 0 {
			state = sensu.CheckStateCritical
		}
		restart(state, "%d/%d worker(s) booted", s.BootedWorkers, s.Workers)
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
			load("busy", sensu.CheckStateCritical, "threads %d/%d busy (%d%%)", busy, capacity, ratio)
			globalAlert = true
		case th.BusyWarn > 0 && ratio >= th.BusyWarn:
			load("busy", sensu.CheckStateWarning, "threads %d/%d busy (%d%%)", busy, capacity, ratio)
			globalAlert = true
		}
	}
	// A single saturated worker hidden by an acceptable average (slow requests piling up on it).
	if !globalAlert && th.BusyCrit > 0 {
		for _, w := range s.WorkerStatus {
			ts := w.LastStatus
			if w.Booted && ts.MaxThreads > 0 && ts.Busy()*100/ts.MaxThreads >= th.BusyCrit {
				load(fmt.Sprintf("worker%d", w.Index), sensu.CheckStateWarning, "worker %d threads %d/%d busy", w.Index, ts.Busy(), ts.MaxThreads)
			}
		}
	}

	switch {
	case th.BacklogCrit > 0 && backlog >= th.BacklogCrit:
		load("backlog", sensu.CheckStateCritical, "backlog %d", backlog)
	case th.BacklogWarn > 0 && backlog >= th.BacklogWarn:
		load("backlog", sensu.CheckStateWarning, "backlog %d", backlog)
	}

	return []string{
		fmt.Sprintf("%d/%d booted", s.BootedWorkers, s.Workers),
		fmt.Sprintf("threads %d/%d busy", busy, capacity),
		fmt.Sprintf("backlog %d", backlog),
	}
}

func pids(ps []Process) string {
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

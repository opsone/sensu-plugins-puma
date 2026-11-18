package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Issue is a stats source that could not be used (unreadable state file, unreachable control app).
// It may hide the stats of an instance: reported only when a checked instance has no stats source.
type Issue struct {
	Source string // "state file", "control app"
	Text   string
}

// stateEntry is a state file found with --state-glob.
type stateEntry struct {
	path      string
	state     State
	noControl bool // no control_url: activate_control_app not set
}

// indexStates reads the state files matching the globs and indexes them by master PID.
// Files that cannot be read are returned as issues: they may hide the stats of an instance.
func indexStates(globs []string) (map[int]stateEntry, []Issue) {
	index := map[int]stateEntry{}
	var issues []Issue
	seen := map[string]bool{}
	for _, g := range globs {
		paths, _ := filepath.Glob(g) // patterns validated by checkArgs
		for _, path := range paths {
			if seen[path] {
				continue
			}
			seen[path] = true
			st, err := ReadState(path)
			noControl := errors.Is(err, ErrNoControlApp)
			switch {
			case err != nil && !noControl:
				issues = append(issues, Issue{"state file", err.Error()})
			case st.PID == 0:
				issues = append(issues, Issue{"state file", path + ": no pid"})
			default:
				// A stale file (instance stopped, app removed) holds a PID no master has: never looked up.
				index[st.PID] = stateEntry{path, st, noControl}
			}
		}
	}
	return index, issues
}

// collector gathers the control app stats of the instances, located through their state file
// (by master PID) or among the --control endpoints (by worker PID).
type collector struct {
	states   map[int]stateEntry
	globs    bool     // state files searched (--state-glob)
	controls []string // control URLs (--control)
	token    string   // control token of the --control endpoints, empty with no_token
	timeout  time.Duration

	polled  bool
	answers []*Stats // answers of the reachable --control endpoints
	issues  []Issue  // unreachable --control endpoints
}

// collect finds the stats of an instance: control app of its state file first, else the --control
// endpoint serving its workers. Without either, the instance is checked from its processes only.
func (c *collector) collect(inst *Instance) Report {
	r := Report{Instance: inst}
	noControl := false
	for _, m := range inst.Masters {
		e, ok := c.states[m.PID]
		if !ok {
			continue
		}
		if !e.noControl {
			r.StatePID = e.state.PID
			ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
			defer cancel()
			r.Stats, r.StatsErr = FetchStats(ctx, e.state)
			return r
		}
		noControl = true
	}
	if len(inst.Masters) == 0 {
		return r // orphan workers: no control app
	}
	if r.Stats = c.match(inst); r.Stats != nil {
		return r
	}

	switch {
	case noControl:
		r.Note = "no control app" // state file found: the instance does not enable activate_control_app
	default:
		var why []string
		if c.globs {
			why = append(why, "no state file")
		}
		if len(c.controls) > 0 {
			why = append(why, "no control app matched")
		}
		r.Note, r.NoState = strings.Join(why, ", "), true
	}
	return r
}

// match returns the answer of the --control endpoint serving one of the workers of the instance.
func (c *collector) match(inst *Instance) *Stats {
	c.poll()
	pids := map[int]bool{}
	for _, w := range inst.Workers {
		pids[w.PID] = true
	}
	for _, s := range c.answers {
		for _, w := range s.WorkerStatus {
			if pids[w.PID] {
				return s
			}
		}
	}
	return nil
}

// poll queries the --control endpoints once, in parallel, when an instance has no state file.
func (c *collector) poll() {
	if c.polled || len(c.controls) == 0 {
		return
	}
	c.polled = true
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	stats := make([]*Stats, len(c.controls))
	errs := make([]error, len(c.controls))
	var wg sync.WaitGroup
	for i, u := range c.controls {
		wg.Go(func() {
			stats[i], errs[i] = FetchStats(ctx, State{ControlURL: u, ControlAuthToken: c.token})
		})
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			c.issues = append(c.issues, Issue{"control app", strings.TrimPrefix(err.Error(), "control app ")})
			continue
		}
		c.answers = append(c.answers, stats[i])
	}
}

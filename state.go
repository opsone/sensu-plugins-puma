package main

import (
	"context"
	"errors"
	"path/filepath"
	"time"
)

// stateEntry is a state file found with --state-glob.
type stateEntry struct {
	path      string
	state     State
	noControl bool // no control_url: activate_control_app not set
}

// indexStates reads the state files matching the globs and indexes them by master PID.
// Files that cannot be read are returned as issues: they may hide the stats of an instance.
func indexStates(globs []string) (map[int]stateEntry, []string) {
	index := map[int]stateEntry{}
	var issues []string
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
				issues = append(issues, err.Error())
			case st.PID == 0:
				issues = append(issues, path+": no pid")
			default:
				// A stale file (instance stopped, app removed) holds a PID no master has: never looked up.
				index[st.PID] = stateEntry{path, st, noControl}
			}
		}
	}
	return index, issues
}

// collector gathers the state file and control app stats of the instances.
type collector struct {
	states  map[int]stateEntry
	timeout time.Duration
}

// collect finds the state file of an instance by master PID and queries its control app.
// Without state file, the instance is checked from its processes only.
func (c collector) collect(inst *Instance) Report {
	r := Report{Instance: inst}
	for _, m := range inst.Masters {
		if e, ok := c.states[m.PID]; ok {
			return c.fetch(r, e.state, e.noControl)
		}
	}
	if len(inst.Masters) > 0 {
		r.Note, r.NoState = "no state file", true
	}
	return r
}

func (c collector) fetch(r Report, st State, noControl bool) Report {
	if noControl {
		r.Note = "no control app"
		return r
	}
	r.StatePID = st.PID
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	r.Stats, r.StatsErr = FetchStats(ctx, st)
	return r
}

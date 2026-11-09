package main

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"time"

	"github.com/opsone/sensu-plugins-puma/internal/puma"
)

// defaultStateFile is looked up in the master working directory when no --state-glob file matches.
const defaultStateFile = "tmp/pids/puma.state"

// stateEntry is a state file found with --state-glob.
type stateEntry struct {
	path      string
	state     puma.State
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
			st, err := puma.ReadState(path)
			noControl := errors.Is(err, puma.ErrNoControlApp)
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
	procRoot string
	states   map[int]stateEntry
	globs    bool // --state-glob given
	timeout  time.Duration
}

// collect finds the state file of an instance (by master PID, else in the master working
// directory) and queries its control app.
func (c collector) collect(inst *puma.Instance) Report {
	r := Report{Instance: inst}
	if len(inst.Masters) == 0 {
		return r
	}

	for _, m := range inst.Masters {
		if e, ok := c.states[m.PID]; ok {
			return c.fetch(r, e.state, e.noControl)
		}
	}

	// Fallback: default location in the working directory of the master, readable as root only.
	cwd, err := puma.Cwd(c.procRoot, inst.Masters[0].PID)
	if err != nil {
		r.Note, r.NoState = "no state file", true
		if !c.globs {
			r.Note = "master cwd unreadable, set --state-glob"
		}
		return r
	}
	st, err := puma.ReadState(filepath.Join(cwd, defaultStateFile))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// The app may simply not use a state file: process checks only.
		r.Note, r.NoState = "no state file", true
		return r
	case errors.Is(err, puma.ErrNoControlApp):
		return c.fetch(r, st, true)
	case err != nil:
		r.StatsErr = err
		return r
	}
	return c.fetch(r, st, false)
}

func (c collector) fetch(r Report, st puma.State, noControl bool) Report {
	if noControl {
		r.Note = "no control app"
		return r
	}
	r.StatePID = st.PID
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	r.Stats, r.StatsErr = puma.FetchStats(ctx, st)
	return r
}

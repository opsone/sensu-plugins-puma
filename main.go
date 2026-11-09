package main

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/opsone/sensu-plugins-puma/internal/puma"
	corev2 "github.com/sensu/core/v2"
	"github.com/sensu/sensu-plugin-sdk/sensu"
)

// Config represents the check plugin config.
type Config struct {
	sensu.PluginConfig
	Expect       []string
	StateGlobs   []string
	MinWorkers   int
	BusyWarn     int
	BusyCrit     int
	BacklogWarn  int
	BacklogCrit  int
	CheckinMax   int
	PhaseMax     int
	WorkerMinAge int
	RSSWarn      int
	RSSCrit      int
	Timeout      int
	ProcRoot     string
}

var (
	plugin = Config{
		PluginConfig: sensu.PluginConfig{
			Name:     "check-puma",
			Short:    "Check the health of the Puma cluster instances running on the host",
			Keyspace: "sensu.io/plugins/check-puma/config",
		},
	}

	options = []sensu.ConfigOption{
		&sensu.SlicePluginConfigOption[string]{
			Path:      "expect",
			Env:       "CHECK_PUMA_EXPECT",
			Argument:  "expect",
			Shorthand: "e",
			Default:   []string{},
			Usage:     "Puma tag to check, {app}_{env} (repeatable or comma separated): must be running, the other instances are ignored. Default: every running instance",
			Value:     &plugin.Expect,
		},
		&sensu.SlicePluginConfigOption[string]{
			Path:      "state-glob",
			Env:       "CHECK_PUMA_STATE_GLOB",
			Argument:  "state-glob",
			Shorthand: "s",
			Default:   []string{},
			Usage:     "Glob of Puma state files, matched to the instances by master PID (repeatable). Fallback: <master cwd>/" + defaultStateFile + ", as root",
			Value:     &plugin.StateGlobs,
		},
		&sensu.PluginConfigOption[int]{
			Path: "min-workers", Env: "CHECK_PUMA_MIN_WORKERS", Argument: "min-workers", Default: 2,
			Usage: "Minimum number of worker processes per master (WARNING below, CRITICAL at 0)",
			Value: &plugin.MinWorkers,
		},
		&sensu.PluginConfigOption[int]{
			Path: "busy-warn", Env: "CHECK_PUMA_BUSY_WARN", Argument: "busy-warn", Default: 80,
			Usage: "Busy threads percentage for WARNING (0 disables)", Value: &plugin.BusyWarn,
		},
		&sensu.PluginConfigOption[int]{
			Path: "busy-crit", Env: "CHECK_PUMA_BUSY_CRIT", Argument: "busy-crit", Default: 95,
			Usage: "Busy threads percentage for CRITICAL (0 disables)", Value: &plugin.BusyCrit,
		},
		&sensu.PluginConfigOption[int]{
			Path: "backlog-warn", Env: "CHECK_PUMA_BACKLOG_WARN", Argument: "backlog-warn", Default: 5,
			Usage: "Queued requests for WARNING, all workers (0 disables)", Value: &plugin.BacklogWarn,
		},
		&sensu.PluginConfigOption[int]{
			Path: "backlog-crit", Env: "CHECK_PUMA_BACKLOG_CRIT", Argument: "backlog-crit", Default: 20,
			Usage: "Queued requests for CRITICAL, all workers (0 disables)", Value: &plugin.BacklogCrit,
		},
		&sensu.PluginConfigOption[int]{
			Path: "checkin-max", Env: "CHECK_PUMA_CHECKIN_MAX", Argument: "checkin-max", Default: 60,
			Usage: "Seconds without worker checkin before CRITICAL (0 disables)", Value: &plugin.CheckinMax,
		},
		&sensu.PluginConfigOption[int]{
			Path: "phase-max", Env: "CHECK_PUMA_PHASE_MAX", Argument: "phase-max", Default: 15,
			Usage: "Minutes a phased restart may last before WARNING (0 disables)", Value: &plugin.PhaseMax,
		},
		&sensu.PluginConfigOption[int]{
			Path: "worker-min-age", Env: "CHECK_PUMA_WORKER_MIN_AGE", Argument: "worker-min-age", Default: 60,
			Usage: "Seconds: a younger worker under an older master is reported as restarted (0 disables)",
			Value: &plugin.WorkerMinAge,
		},
		&sensu.PluginConfigOption[int]{
			Path: "rss-warn", Env: "CHECK_PUMA_RSS_WARN", Argument: "rss-warn", Default: 0,
			Usage: "Worker RSS in MB for WARNING (0 disables)", Value: &plugin.RSSWarn,
		},
		&sensu.PluginConfigOption[int]{
			Path: "rss-crit", Env: "CHECK_PUMA_RSS_CRIT", Argument: "rss-crit", Default: 0,
			Usage: "Worker RSS in MB for CRITICAL (0 disables)", Value: &plugin.RSSCrit,
		},
		&sensu.PluginConfigOption[int]{
			Path: "timeout", Env: "CHECK_PUMA_TIMEOUT", Argument: "timeout", Default: 5,
			Usage: "Control app query timeout in seconds, per instance", Value: &plugin.Timeout,
		},
		&sensu.PluginConfigOption[string]{
			Path: "proc-root", Env: "CHECK_PUMA_PROC_ROOT", Argument: "proc-root", Default: "/proc",
			Usage: "procfs mount point", Value: &plugin.ProcRoot,
		},
	}
)

func main() {
	check := sensu.NewGoCheck(&plugin.PluginConfig, options, checkArgs, executeCheck, false)
	check.Execute()
}

func checkArgs(event *corev2.Event) (int, error) {
	for name, v := range map[string]int{
		"min-workers": plugin.MinWorkers, "busy-warn": plugin.BusyWarn, "busy-crit": plugin.BusyCrit,
		"backlog-warn": plugin.BacklogWarn, "backlog-crit": plugin.BacklogCrit, "checkin-max": plugin.CheckinMax,
		"phase-max": plugin.PhaseMax, "worker-min-age": plugin.WorkerMinAge,
		"rss-warn": plugin.RSSWarn, "rss-crit": plugin.RSSCrit,
	} {
		if v < 0 {
			return sensu.CheckStateUnknown, fmt.Errorf("--%s must not be negative", name)
		}
	}
	if plugin.Timeout <= 0 {
		return sensu.CheckStateUnknown, fmt.Errorf("--timeout must be greater than 0")
	}
	for _, g := range plugin.StateGlobs {
		if _, err := filepath.Match(g, ""); err != nil {
			return sensu.CheckStateUnknown, fmt.Errorf("invalid --state-glob %q: %w", g, err)
		}
	}
	return sensu.CheckStateOK, nil
}

func executeCheck(event *corev2.Event) (int, error) {
	instances, err := puma.Discover(plugin.ProcRoot)
	if err != nil {
		return sensu.CheckStateUnknown, fmt.Errorf("scanning %s: %w", plugin.ProcRoot, err)
	}

	states, issues := indexStates(plugin.StateGlobs)
	c := collector{
		procRoot: plugin.ProcRoot,
		states:   states,
		globs:    len(plugin.StateGlobs) > 0,
		timeout:  time.Duration(plugin.Timeout) * time.Second,
	}
	expected := expectedTags(plugin.Expect)
	reports := make([]Report, 0, len(instances))
	for _, inst := range instances {
		if len(expected) > 0 && !slices.Contains(expected, inst.Tag) {
			reports = append(reports, Report{Instance: inst}) // ignored: no control app query
			continue
		}
		reports = append(reports, c.collect(inst))
	}

	result := evaluate(expected, reports, thresholds(), time.Now(), issues...)
	fmt.Printf("%s %s: %s\n", plugin.Name, stateLabel(result.State), result.Message)
	return result.State, nil
}

// expectedTags flattens the --expect values; empty items come from an unset Sensu token.
func expectedTags(values []string) []string {
	var tags []string
	for _, v := range values {
		for _, t := range strings.Split(v, ",") {
			if t = strings.TrimSpace(t); t != "" {
				tags = append(tags, t)
			}
		}
	}
	return tags
}

func thresholds() Thresholds {
	return Thresholds{
		MinWorkers:   plugin.MinWorkers,
		BusyWarn:     plugin.BusyWarn,
		BusyCrit:     plugin.BusyCrit,
		BacklogWarn:  plugin.BacklogWarn,
		BacklogCrit:  plugin.BacklogCrit,
		CheckinMax:   time.Duration(plugin.CheckinMax) * time.Second,
		PhaseMax:     time.Duration(plugin.PhaseMax) * time.Minute,
		WorkerMinAge: time.Duration(plugin.WorkerMinAge) * time.Second,
		RSSWarn:      uint64(plugin.RSSWarn) * 1024 * 1024,
		RSSCrit:      uint64(plugin.RSSCrit) * 1024 * 1024,
	}
}

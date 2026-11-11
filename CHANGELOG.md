# Changelog

## [0.4.0] - 2026-11-11

### Added
- `--restart-grace` (default 300 seconds): instance or master not running, orphan workers, missing worker processes, workers not booted and control app unreachable are only reported after this delay, as one restart episode per instance.
- `--load-grace` (default 180 seconds): busy threads, saturated worker and backlog are only reported when they last this delay.
- `--state-dir` (default `/var/cache/sensu/sensu-agent`): state file of the grace delays. Without usable state file, the conditions are reported at once with a `grace WARNING` line.

### Changed
- Young workers are no longer reported as restarted when every worker of the instance is young (hot restart).
- A condition within its grace delay is shown on the OK line of the instance (`… for 40s, within grace`).

## [0.3.1] - 2026-11-10

### Changed
- The instances left out by `--expect` are no longer named in the output (`ignored:` line removed).

## [0.3.0] - 2026-11-09

### Changed
- Renamed `check-puma` to `check-puma-status`, after the naming rule of the Opsone plugins (the check reads the Puma status page).
- `--state-glob` is required: the fallback on `tmp/pids/puma.state` in the master working directory (root only) is removed.
- README: every option passed through `env_vars` filled from entity annotations, as in the other Opsone plugins, with a table of the environment variables.

### Added
- `--expect` accepts patterns (`*_prod`): the matching running instances are checked, without being required. `*` checks every running instance.

### Fixed
- `CHECK_PUMA_STATE_GLOB` holding several comma separated globs was read as a single glob.

## [0.2.0] - 2026-11-09

### Changed
- `--state-pattern` replaced by `--state-glob` (repeatable): state files are matched to the instances by master PID, the deployment directory no longer needs to match the Puma tag. Stale state files are ignored, unreadable ones are a warning when a checked instance has no state file.
- `--expect` lists the instances to check: the other running instances are ignored (named on an `ignored:` line) instead of being checked. Without `--expect`, every running instance is still checked.

## [0.1.0] - 2026-11-08

### Added
- Discovery of the Puma cluster instances from their process title (`--tag {app}_{env}`), `--expect` for the instances that must run.
- Process checks: master, worker count (`--min-workers`), restarted workers, worker RSS.
- Control app checks (`/stats`, located through the state file): booted workers, hung workers (checkin), stuck phased restart, busy threads, backlog.

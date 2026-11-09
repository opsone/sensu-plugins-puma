# Changelog

## [Unreleased]

### Changed
- `--state-pattern` replaced by `--state-glob` (repeatable): state files are matched to the instances by master PID, the deployment directory no longer needs to match the Puma tag. Stale state files are ignored, unreadable ones are a warning when a checked instance has no state file.
- `--expect` lists the instances to check: the other running instances are ignored (named on an `ignored:` line) instead of being checked. Without `--expect`, every running instance is still checked.

## [0.1.0] - 2026-11-08

### Added
- Discovery of the Puma cluster instances from their process title (`--tag {app}_{env}`), `--expect` for the instances that must run.
- Process checks: master, worker count (`--min-workers`), restarted workers, worker RSS.
- Control app checks (`/stats`, located through the state file): booted workers, hung workers (checkin), stuck phased restart, busy threads, backlog.

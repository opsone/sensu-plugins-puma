# Changelog

## [0.1.0] - 2026-11-08

### Added
- Discovery of the Puma cluster instances from their process title (`--tag {app}_{env}`), `--expect` for the instances that must run.
- Process checks: master, worker count (`--min-workers`), restarted workers, worker RSS.
- Control app checks (`/stats`, located through the state file): booted workers, hung workers (checkin), stuck phased restart, busy threads, backlog.

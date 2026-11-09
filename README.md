# check-puma-status

Sensu Go check for the Puma (Ruby on Rails) cluster instances running on a host, several instances per host supported (e.g. `myapp_prod` and `myapp_staging`).

Instances are discovered from the process titles, which carry the Puma tag (`--tag {app}_{env}`):

```
puma 8.0.2 (tcp://127.0.0.1:3000) [myapp_prod]
puma: cluster worker 0: 12345 [myapp_prod]
puma: cluster worker 1: 12345 [myapp_prod]
```

The thread pool state comes from the Puma control app (`/stats`), located through the instance state file. No CGO, static binary, Linux only (`/proc`).

## Behavior

From the processes (always available):

| Situation | State |
|---|---|
| Instance given with `--expect` not running | CRITICAL |
| Workers without master | CRITICAL |
| No worker process / fewer than `--min-workers` (default 2) | CRITICAL / WARNING |
| Several masters with the same tag | WARNING |
| Worker younger than `--worker-min-age` under an older master (crash, killed) | WARNING |
| Worker RSS above `--rss-warn` / `--rss-crit` | WARNING / CRITICAL |

From the control app, when the state file is found:

| Situation | State |
|---|---|
| Control app unreachable | WARNING |
| State file matching `--state-glob` unreadable | WARNING |
| No worker booted / some workers not booted | CRITICAL / WARNING |
| Worker without checkin for `--checkin-max` seconds (hung) | CRITICAL |
| Phased restart lasting more than `--phase-max` minutes | WARNING |
| Busy threads (all workers) above `--busy-warn` / `--busy-crit` | WARNING / CRITICAL |
| One worker above `--busy-crit` while the average is fine | WARNING |
| Queued requests (all workers) above `--backlog-warn` / `--backlog-crit` | WARNING / CRITICAL |
| Instance not in cluster mode | WARNING |

An instance without state file or without control app is checked from its processes only, and stays OK.

`--expect` lists the instances to check: the other running instances (e.g. a staging not worth an alert) are ignored, only named on an `ignored:` line.

- A name (`myapp_prod`) must be running: CRITICAL if not.
- A pattern (`*_prod`, `myapp_*`) checks the running instances it matches, without requiring any: a stopped instance goes unnoticed.
- Without `--expect` (or `*`), every running instance is checked.

The output gives one line per instance, the state is the worst one:

```
check-puma-status CRITICAL: 1/2 instance(s) in trouble
myapp_api_prod OK: 2 worker(s), 2/2 booted, threads 1/4 busy, backlog 0, rss max 312MB
myapp_prod CRITICAL: threads 4/4 busy (100%)
ignored: myapp_staging
```

Workers report their thread pool to the master at checkin, every 5 seconds: busy threads and backlog are a 5-second-old snapshot.

## Puma configuration

`config/puma.rb`:

```ruby
# Monitoring (check-puma-status): state file and read-only control app
if ENV['PUMA_CONTROL_PORT']
  state_path 'tmp/pids/puma.state'
  activate_control_app "tcp://127.0.0.1:#{ENV['PUMA_CONTROL_PORT']}", data_only: true
end
```

- One control port per instance, set in its systemd unit (`Environment=PUMA_CONTROL_PORT=9293`): `myapp_prod` and `myapp_staging` on the same host need different ports. Convention: `9293` (the `pumactl` default) for the first instance of a host, then `9294`, `9295`… for the next ones. Avoid `3000`/`3001`, often taken by the applications themselves. The check reads the URL from the state file, it does not need to know the ports.
- `data_only: true` restricts the control app to the read-only endpoints (`/stats`, `/gc-stats`…): without it, `/stop`, `/halt` or `/restart` are reachable by any local user holding the token.
- No `auth_token`: Puma generates one at boot and writes it in the state file, where the check reads it.
- `tmp/pids` must be shared between releases (Capistrano `linked_dirs`).

A unix socket control app (`unix://tmp/pids/pumactl.sock`) is supported too, but the Sensu agent then needs write permission on the socket.

Puma is started in cluster mode with `--tag {app}_{env}`. The env is what follows the last underscore: `my_app_prod` → app `my_app`, env `prod`.

## State file location

The state files are given as globs with `--state-glob` (required, repeatable), and matched to the instances by **master PID** (the `pid` of the state file): the deployment directory does not need to match the Puma tag.

```sh
check-puma-status --state-glob '/home/deploy/*/shared/tmp/pids/puma.state' \
                  --state-glob '/var/www/*/shared/tmp/pids/puma.state'
```

- A stale state file (instance stopped, app removed) holds a PID no master has: it is ignored.
- A matching state file that cannot be read (permissions, no `pid`) is a WARNING when a checked instance has no state file: it may be its one.
- Globs are standard shell patterns, `**` is not supported.
- An instance without matching state file is checked from its processes only.

With a TCP control app, the Sensu agent user only needs to read the state files (they hold the control token) and to traverse their parent directories. Puma writes them with the process umask, usually world readable. To check:

```sh
sudo -u sensu check-puma-status --state-glob '/home/deploy/*/shared/tmp/pids/puma.state' --expect myapp_prod
```

## Usage

```
check-puma-status --state-glob <glob> [flags]
```

| Flag | Environment variable | Default | Description |
|---|---|---|---|
| `-s`, `--state-glob` | `CHECK_PUMA_STATE_GLOB` | required | Glob of Puma state files, matched to the instances by master PID |
| `-e`, `--expect` | `CHECK_PUMA_EXPECT` | every instance | Puma tags (must run) or patterns (`*_prod`) to check, the others are ignored |
| `--min-workers` | `CHECK_PUMA_MIN_WORKERS` | `2` | Minimum worker processes per master (WARNING below, CRITICAL at 0) |
| `--busy-warn` | `CHECK_PUMA_BUSY_WARN` | `80` | Busy threads %, all workers, for WARNING |
| `--busy-crit` | `CHECK_PUMA_BUSY_CRIT` | `95` | Busy threads %, all workers, for CRITICAL |
| `--backlog-warn` | `CHECK_PUMA_BACKLOG_WARN` | `5` | Queued requests, all workers, for WARNING |
| `--backlog-crit` | `CHECK_PUMA_BACKLOG_CRIT` | `20` | Queued requests, all workers, for CRITICAL |
| `--checkin-max` | `CHECK_PUMA_CHECKIN_MAX` | `60` | Seconds without worker checkin before CRITICAL |
| `--phase-max` | `CHECK_PUMA_PHASE_MAX` | `15` | Minutes a phased restart may last before WARNING |
| `--worker-min-age` | `CHECK_PUMA_WORKER_MIN_AGE` | `60` | Seconds under which a worker under an older master is reported as restarted |
| `--rss-warn` | `CHECK_PUMA_RSS_WARN` | `0` | Worker RSS in MB for WARNING |
| `--rss-crit` | `CHECK_PUMA_RSS_CRIT` | `0` | Worker RSS in MB for CRITICAL |
| `--timeout` | `CHECK_PUMA_TIMEOUT` | `5` | Control app query timeout in seconds, per instance |
| `--proc-root` | `CHECK_PUMA_PROC_ROOT` | `/proc` | procfs mount point |

A threshold set to 0 disables its test. Lists (`--state-glob`, `--expect`) are repeatable or comma separated, also in their environment variable. A missing `--state-glob` or an invalid pattern is UNKNOWN.

## Check definition

Every option is passed through `env_vars`, filled per host from entity annotations with token substitution (applied by the agent to the whole check definition, `env_vars` included). One annotation maps to one option.

Naming rule: an annotation is named after its environment variable, lowercased, without the `CHECK_` prefix (`CHECK_PUMA_RSS_CRIT` → `puma_rss_crit`).

Every annotation is optional: each token has an explicit, non-empty `default` in the check definition, matching the plugin defaults, so a host without annotations runs the check with the values below. Never use `| default ""`: Sensu rejects an empty environment variable, and the agent then silently drops the check. `puma_expect` therefore defaults to `*`, every running instance.

Lists (`puma_expect`, `puma_state_glob`) are comma separated.

Annotations, e.g. in the agent configuration (`/etc/sensu/agent.yml`):

```yaml
annotations:
  puma_expect: "myapp_prod,myapp_api_prod"   # names must run, patterns allowed (*_prod), default * (all)
  puma_state_glob: "/home/deploy/*/shared/tmp/pids/puma.state,/var/www/*/shared/tmp/pids/puma.state"
  puma_min_workers: "2"                      # default 2
  puma_busy_warn: "80"                       # %, default 80
  puma_busy_crit: "95"                       # %, default 95
  puma_backlog_warn: "5"                     # default 5
  puma_backlog_crit: "20"                    # default 20
  puma_checkin_max: "60"                     # seconds, default 60
  puma_phase_max: "15"                       # minutes, default 15
  puma_worker_min_age: "60"                  # seconds, default 60
  puma_rss_warn: "1200"                      # MB per worker, default 0 (disabled)
  puma_rss_crit: "1500"                      # MB per worker, default 0 (disabled)
  puma_timeout: "5"                          # seconds, default 5
```

```yaml
---
type: CheckConfig
api_version: core/v2
metadata:
  name: check-puma-status
spec:
  command: check-puma-status
  env_vars:
    - CHECK_PUMA_EXPECT={{ .annotations.puma_expect | default "*" }}
    - CHECK_PUMA_STATE_GLOB={{ .annotations.puma_state_glob | default "/home/deploy/*/shared/tmp/pids/puma.state" }}
    - CHECK_PUMA_MIN_WORKERS={{ .annotations.puma_min_workers | default "2" }}
    - CHECK_PUMA_BUSY_WARN={{ .annotations.puma_busy_warn | default "80" }}
    - CHECK_PUMA_BUSY_CRIT={{ .annotations.puma_busy_crit | default "95" }}
    - CHECK_PUMA_BACKLOG_WARN={{ .annotations.puma_backlog_warn | default "5" }}
    - CHECK_PUMA_BACKLOG_CRIT={{ .annotations.puma_backlog_crit | default "20" }}
    - CHECK_PUMA_CHECKIN_MAX={{ .annotations.puma_checkin_max | default "60" }}
    - CHECK_PUMA_PHASE_MAX={{ .annotations.puma_phase_max | default "15" }}
    - CHECK_PUMA_WORKER_MIN_AGE={{ .annotations.puma_worker_min_age | default "60" }}
    - CHECK_PUMA_RSS_WARN={{ .annotations.puma_rss_warn | default "0" }}
    - CHECK_PUMA_RSS_CRIT={{ .annotations.puma_rss_crit | default "0" }}
    - CHECK_PUMA_TIMEOUT={{ .annotations.puma_timeout | default "5" }}
  runtime_assets:
    - opsone/sensu-plugins-puma
  interval: 60
  timeout: 30
  publish: true
  subscriptions:
    - puma
```

## Build and release

```sh
make test          # go vet + unit tests
make integration   # runs the check against real Puma cluster instances in Docker
```

Release:

```sh
git tag 0.1.0 && git push origin 0.1.0
make release       # version taken from the tag, or forced: make release VERSION=0.1.0
sensuctl create -f dist/opsone-sensu-plugins-puma_0.1.0.yml
```

`make release` runs `make dist`, which runs the tests then produces in `dist/` the `linux/amd64` archive in Sensu asset layout (`bin/check-puma-status`), the sha512 checksums file and the asset definition, then creates the GitHub release of the tag with `gh`.

`check-puma-status version` prints the version, commit and build date.

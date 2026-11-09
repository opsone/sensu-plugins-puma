# check-puma

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

`--expect` lists the instances to check: the other running instances (e.g. a staging not worth an alert) are ignored, only named on an `ignored:` line. Without `--expect`, every running instance is checked, but a stopped one goes unnoticed.

The output gives one line per instance, the state is the worst one:

```
check-puma CRITICAL: 1/2 instance(s) in trouble
myapp_api_prod OK: 2 worker(s), 2/2 booted, threads 1/4 busy, backlog 0, rss max 312MB
myapp_prod CRITICAL: threads 4/4 busy (100%)
ignored: myapp_staging
```

Workers report their thread pool to the master at checkin, every 5 seconds: busy threads and backlog are a 5-second-old snapshot.

## Puma configuration

`config/puma.rb`:

```ruby
# Monitoring (check-puma): state file and read-only control app
if ENV['PUMA_CONTROL_PORT']
  state_path 'tmp/pids/puma.state'
  activate_control_app "tcp://127.0.0.1:#{ENV['PUMA_CONTROL_PORT']}", data_only: true
end
```

- One control port per instance, set in its systemd unit (`Environment=PUMA_CONTROL_PORT=9293`): `myapp_prod` and `myapp_staging` on the same host need different ports. The check reads the URL from the state file, it does not need to know the ports.
- `data_only: true` restricts the control app to the read-only endpoints (`/stats`, `/gc-stats`…): without it, `/stop`, `/halt` or `/restart` are reachable by any local user holding the token.
- No `auth_token`: Puma generates one at boot and writes it in the state file, where the check reads it.
- `tmp/pids` must be shared between releases (Capistrano `linked_dirs`).

A unix socket control app (`unix://tmp/pids/pumactl.sock`) is supported too, but the Sensu agent then needs write permission on the socket.

Puma is started in cluster mode with `--tag {app}_{env}`. The env is what follows the last underscore: `my_app_prod` → app `my_app`, env `prod`.

## State file location

The state files are given as globs with `--state-glob` (repeatable), and matched to the instances by **master PID** (the `pid` of the state file): the deployment directory does not need to match the Puma tag.

```sh
check-puma --state-glob '/home/deploy/*/shared/tmp/pids/puma.state' \
           --state-glob '/var/www/*/shared/tmp/pids/puma.state'
```

- A stale state file (instance stopped, app removed) holds a PID no master has: it is ignored.
- A matching state file that cannot be read (permissions, no `pid`) is a WARNING when a checked instance has no state file: it may be its one.
- Globs are standard shell patterns, `**` is not supported.

Without match, the check looks for `tmp/pids/puma.state` in the working directory of the master. Reading the working directory of another user's process requires root: for an unprivileged Sensu agent, use `--state-glob`.

With a TCP control app, the Sensu agent user only needs to read the state files (they hold the control token) and to traverse their parent directories. Puma writes them with the process umask, usually world readable. To check:

```sh
sudo -u sensu check-puma --state-glob '/home/deploy/*/shared/tmp/pids/puma.state' --expect myapp_prod
```

## Usage

```
check-puma [flags]

  -e, --expect strings         Puma tags to check, must be running, others ignored (repeatable or comma separated)
  -s, --state-glob strings     Glob of Puma state files, matched by master PID (repeatable)
      --min-workers int        Minimum worker processes per master (default 2)
      --busy-warn int          Busy threads % for WARNING (default 80)
      --busy-crit int          Busy threads % for CRITICAL (default 95)
      --backlog-warn int       Queued requests for WARNING (default 5)
      --backlog-crit int       Queued requests for CRITICAL (default 20)
      --checkin-max int        Seconds without worker checkin before CRITICAL (default 60)
      --phase-max int          Minutes a phased restart may last before WARNING (default 15)
      --worker-min-age int     Seconds under which a worker is reported as restarted (default 60)
      --rss-warn int           Worker RSS in MB for WARNING (default 0, disabled)
      --rss-crit int           Worker RSS in MB for CRITICAL (default 0, disabled)
      --timeout int            Control app query timeout in seconds, per instance (default 5)
      --proc-root string       procfs mount point (default "/proc")
```

A threshold set to 0 disables its test. Every option can also be set with an environment variable (`CHECK_PUMA_EXPECT`, `CHECK_PUMA_STATE_GLOB`, `CHECK_PUMA_BUSY_WARN`…).

## Check definition

```yaml
---
type: CheckConfig
api_version: core/v2
metadata:
  name: check-puma
spec:
  command: >-
    check-puma
    --state-glob '/home/deploy/*/shared/tmp/pids/puma.state'
    --expect '{{ .labels.puma_instances | default "" }}'
    {{ .annotations.check_puma_args | default "" }}
  runtime_assets:
    - opsone/sensu-plugins-puma
  interval: 60
  timeout: 30
  publish: true
  subscriptions:
    - puma
```

The state globs follow the deployment conventions of the fleet: set once in the check, not per host. Add a `--state-glob` per other convention.

Per-host settings, in the agent configuration:

```yaml
labels:
  puma_instances: "myapp_prod"   # instances to check: myapp_staging, also running, is ignored
annotations:
  check_puma_args: "--rss-crit 1500"
```

`puma_instances` is a label, to select the entities running an app (`sensuctl entity list --label-selector 'puma_instances matches myapp_prod'`, filters, dashboard). `check_puma_args` is an annotation: free-form arguments, not meant for selection.

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

`make release` runs `make dist`, which runs the tests then produces in `dist/` the `linux/amd64` archive in Sensu asset layout (`bin/check-puma`), the sha512 checksums file and the asset definition, then creates the GitHub release of the tag with `gh`.

`check-puma version` prints the version, commit and build date.

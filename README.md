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
| Control app unreachable, state file unreadable | WARNING |
| No worker booted / some workers not booted | CRITICAL / WARNING |
| Worker without checkin for `--checkin-max` seconds (hung) | CRITICAL |
| Phased restart lasting more than `--phase-max` minutes | WARNING |
| Busy threads (all workers) above `--busy-warn` / `--busy-crit` | WARNING / CRITICAL |
| One worker above `--busy-crit` while the average is fine | WARNING |
| Queued requests (all workers) above `--backlog-warn` / `--backlog-crit` | WARNING / CRITICAL |
| Instance not in cluster mode | WARNING |

An instance without state file or without control app is checked from its processes only, and stays OK. A running instance missing from `--expect` is checked and flagged `(not expected)`, without alert.

The output gives one line per instance, the state is the worst one:

```
check-puma CRITICAL: 1/2 instance(s) in trouble
myapp_prod CRITICAL: threads 4/4 busy (100%)
myapp_staging OK: 2 worker(s), 2/2 booted, threads 1/4 busy, backlog 0, rss max 312MB
```

Workers report their thread pool to the master at checkin, every 5 seconds: busy threads and backlog are a 5-second-old snapshot.

## Puma configuration

`config/puma.rb`:

```ruby
state_path "tmp/pids/puma.state"
activate_control_app "unix://tmp/sockets/pumactl.sock", auth_token: ENV.fetch("PUMA_CONTROL_TOKEN")
```

Started with `--tag {app}_{env}`. The env is what follows the last underscore: `my_app_prod` → app `my_app`, env `prod`.

## State file location

By default, `tmp/pids/puma.state` in the working directory of the master. Reading the working directory of another user's process requires root: for an unprivileged Sensu agent, give the path with `--state-pattern`, where `{tag}`, `{app}` and `{env}` are replaced:

```sh
check-puma --state-pattern '/var/www/{tag}/shared/tmp/pids/puma.state'
check-puma --state-pattern '/home/{app}/{env}/shared/tmp/pids/puma.state'
```

The Sensu agent user must be able to read the state file (it holds the control token) and to connect to the control socket (write permission on a unix socket): add it to the group of the application user, or bind the control app on `tcp://127.0.0.1:<port>`.

## Usage

```
check-puma [flags]

  -e, --expect strings         Puma tag that must be running (repeatable or comma separated)
  -s, --state-pattern string   Path of the Puma state file, {tag} {app} {env} replaced
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

A threshold set to 0 disables its test. Every option can also be set with an environment variable (`CHECK_PUMA_EXPECT`, `CHECK_PUMA_STATE_PATTERN`, `CHECK_PUMA_BUSY_WARN`…).

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

Per-host settings, in the agent configuration:

```yaml
labels:
  puma_instances: "myapp_prod,myapp_staging"
annotations:
  check_puma_args: "--state-pattern '/var/www/{tag}/shared/tmp/pids/puma.state' --rss-crit 1500"
```

Without `puma_instances`, the running instances are checked but a stopped one goes unnoticed.

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

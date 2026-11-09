#!/usr/bin/env bash
# Runs check-puma-status against real Puma cluster instances inside a Docker container.
set -euo pipefail
cd "$(dirname "$0")"

image=check-puma-integration
container=check-puma-integration-$$

docker build -q -t "$image" . >/dev/null
docker run -d --rm --name "$container" "$image" >/dev/null
trap 'docker stop -t 1 "$container" >/dev/null' EXIT

dx() { docker exec "$container" "$@"; }

start() { # <tag> <directory> <port> [control port]
  local env=(-e PORT="$3")
  [[ -n ${4:-} ]] && env+=(-e PUMA_CONTROL_PORT="$4")
  docker exec -d -w "$2" "${env[@]}" "$container" puma -C config/puma.rb --tag "$1"
}

# Waits until both workers of an instance are listed.
wait_workers() { # <tag>
  for _ in $(seq 30); do
    [[ $(dx pgrep -fc "cluster worker .* \[$1\]") == 2 ]] && return
    sleep 1
  done
  echo "timeout waiting for $1 workers" >&2
  dx ps -eo pid,args >&2
  exit 1
}

echo "$(dx puma --version)"
# Staging runs without control app: the check falls back on process checks.
start myapp_prod /srv/myapp-production 3000 9293
start myapp_staging /srv/myapp-staging 3001
wait_workers myapp_prod
wait_workers myapp_staging
sleep 2 # first checkin of the workers

failures=0
# Runs the check as an unprivileged user, like sensu-agent does.
expect() { # <exit code> [VAR=value...] <args>...
  local want=$1 env=(); shift
  while [[ ${1:-} == CHECK_*=* ]]; do env+=("$1"); shift; done
  local out rc=0
  out=$(dx runuser -u nobody -- env ${env[@]+"${env[@]}"} check-puma-status "$@") || rc=$?
  if [[ $rc -eq $want ]]; then
    echo "ok   [$rc] ${env[*]+${env[*]}} check-puma-status $*"
  else
    echo "FAIL [$rc, want $want] ${env[*]+${env[*]}} check-puma-status $*"
    failures=$((failures + 1))
  fi
  sed 's/^/       /' <<<"$out"
}

# State files matched by master PID: deployment directories unrelated to the tags,
# stale file of /srv/oldapp ignored. Stats through the TCP control app.
glob='/srv/*/tmp/pids/puma.state'

expect 0 -s "$glob" --expect myapp_prod,myapp_staging
expect 2 -s "$glob" --expect myapp_prod,myapp_staging,other_prod

# Invalid configuration.
expect 3 --expect myapp_prod
expect 3 -s "$glob" --expect 'myapp_[prod'

# Glob matching nothing: process checks only, still OK.
expect 0 -s '/nowhere/*/puma.state' --expect myapp_prod

# Configuration through environment variables, as in the README CheckConfig (comma separated
# lists, expect pattern).
expect 0 CHECK_PUMA_STATE_GLOB="/home/deploy/*/shared/tmp/pids/puma.state,$glob" CHECK_PUMA_EXPECT='*_prod'
expect 2 CHECK_PUMA_STATE_GLOB="$glob" CHECK_PUMA_EXPECT='*_prod,other_prod'

# data_only: the control app refuses the commands changing the server state.
token=$(dx sed -n 's/^control_auth_token: *//p' /srv/myapp-production/tmp/pids/puma.state)
code=$(dx curl -s -o /dev/null -w '%{http_code}' "localhost:9293/stop?token=$token")
if [[ $code == 200 ]]; then
  echo "FAIL control app /stop answered $code despite data_only"; failures=$((failures + 1))
else
  echo "ok   control app /stop refused ($code)"
fi

# Thread saturation: 2 workers x 2 threads, 4 slow requests (8s). Workers report
# their thread pool at checkin (every 5s): check after the next one.
for _ in $(seq 4); do docker exec -d "$container" curl -s localhost:3000/sleep; done
sleep 6
expect 2 -s "$glob" --expect myapp_prod
sleep 3

# Hung worker: no checkin while stopped (Puma workers check in every 5s).
worker=$(dx pgrep -f "cluster worker 0: .* \[myapp_prod\]")
dx kill -STOP "$worker"
sleep 12
expect 2 -s "$glob" --expect myapp_prod --checkin-max 8
dx kill -CONT "$worker"
sleep 6

# Crashed worker: respawned by the master, reported as restarted.
dx kill -KILL "$(dx pgrep -f "cluster worker 1: .* \[myapp_prod\]")"
wait_workers myapp_prod
expect 1 -s "$glob" --expect myapp_prod --worker-min-age 15

# Staging in trouble (crashed worker): alert without --expect, ignored when only prod is expected.
sleep 6 # prod worker restarted above gets older than --worker-min-age
dx kill -KILL "$(dx pgrep -f "cluster worker 1: .* \[myapp_staging\]")"
wait_workers myapp_staging
expect 1 -s "$glob" --worker-min-age 5
expect 0 -s "$glob" --expect myapp_prod --worker-min-age 5

# Master killed: its workers exit, the instance is gone.
dx pkill -KILL -f "puma .* \[myapp_staging\]$"
sleep 3
expect 2 -s "$glob" --expect myapp_prod,myapp_staging

exit $failures

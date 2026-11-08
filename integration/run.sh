#!/usr/bin/env bash
# Runs check-puma against real Puma cluster instances inside a Docker container.
set -euo pipefail
cd "$(dirname "$0")"

image=check-puma-integration
container=check-puma-integration-$$

docker build -q -t "$image" . >/dev/null
docker run -d --rm --name "$container" "$image" >/dev/null
trap 'docker stop -t 1 "$container" >/dev/null' EXIT

dx() { docker exec "$container" "$@"; }

start() { # <tag> <port>
  docker exec -d -w "/srv/$1" -e PORT="$2" "$container" puma -C config/puma.rb --tag "$1"
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
start myapp_prod 3000
start myapp_staging 3001
wait_workers myapp_prod
wait_workers myapp_staging
sleep 2 # first checkin of the workers

failures=0
expect() { # <exit code> [user:]<args>...
  local want=$1 user=root; shift
  if [[ $1 == --as=* ]]; then user=${1#--as=}; shift; fi
  local out rc=0
  out=$(dx runuser -u "$user" -- check-puma "$@") || rc=$?
  if [[ $rc -eq $want ]]; then
    echo "ok   [$rc] check-puma $*"
  else
    echo "FAIL [$rc, want $want] check-puma $*"
    failures=$((failures + 1))
  fi
  sed 's/^/       /' <<<"$out"
}

expect 0 --expect myapp_prod,myapp_staging
expect 2 --expect myapp_prod,myapp_staging,other_prod

# Unprivileged agent: master cwd unreadable, stats skipped, process checks only.
expect 0 --as=nobody --expect myapp_prod,myapp_staging

# Thread saturation: 2 workers x 2 threads, 4 slow requests (8s). Workers report
# their thread pool at checkin (every 5s): check after the next one.
for _ in $(seq 4); do docker exec -d "$container" curl -s localhost:3000/sleep; done
sleep 6
expect 2 --expect myapp_prod
sleep 3

# Hung worker: no checkin while stopped (Puma workers check in every 5s).
worker=$(dx pgrep -f "cluster worker 0: .* \[myapp_prod\]")
dx kill -STOP "$worker"
sleep 12
expect 2 --expect myapp_prod --checkin-max 8
dx kill -CONT "$worker"
sleep 6

# Crashed worker: respawned by the master, reported as restarted.
dx kill -KILL "$(dx pgrep -f "cluster worker 1: .* \[myapp_prod\]")"
wait_workers myapp_prod
expect 1 --expect myapp_prod --worker-min-age 15

# Master killed: its workers exit, the instance is gone.
dx pkill -KILL -f "puma .* \[myapp_staging\]$"
sleep 3
expect 2 --expect myapp_prod,myapp_staging

exit $failures

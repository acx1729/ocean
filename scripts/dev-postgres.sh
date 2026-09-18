#!/usr/bin/env bash
# A disposable Postgres 16 cluster for local development and tests, on
# 127.0.0.1:55432 with a passwordless "postgres" superuser (the defaults the
# Go tests, the e2e runner and docs/running-locally.md assume).
#
#   scripts/dev-postgres.sh start    # pg_ctl if a local Postgres 16 is installed, else Docker
#   scripts/dev-postgres.sh stop
#   scripts/dev-postgres.sh status
#   scripts/dev-postgres.sh reset    # stop, wipe the data directory, start again
#
# Override PGPORT, KB_DEV_PGDATA or KB_DEV_PG_BIN as needed. The cluster is
# tuned for tests (fsync off); never point production data at it.
set -euo pipefail

PGPORT="${PGPORT:-55432}"
PGDATA="${KB_DEV_PGDATA:-${TMPDIR:-/tmp}/kb-pg/data}"
PGBIN="${KB_DEV_PG_BIN:-}"
CONTAINER="kb-dev-postgres"
SOCKET_DIR="$(dirname "$PGDATA")"
DSN="postgres://postgres@127.0.0.1:${PGPORT}/postgres?sslmode=disable"

find_pgbin() {
  if [ -n "$PGBIN" ] && [ -x "$PGBIN/pg_ctl" ]; then echo "$PGBIN"; return; fi
  for d in /usr/lib/postgresql/16/bin /usr/local/opt/postgresql@16/bin /opt/homebrew/opt/postgresql@16/bin /usr/pgsql-16/bin; do
    if [ -x "$d/pg_ctl" ]; then echo "$d"; return; fi
  done
  if command -v pg_ctl >/dev/null 2>&1; then dirname "$(command -v pg_ctl)"; return; fi
  echo ""
}

run_as_pg() {
  # initdb refuses to run as root; hand off to the postgres user when needed.
  if [ "$(id -u)" = "0" ] && id postgres >/dev/null 2>&1; then
    runuser -u postgres -- "$@"
  else
    "$@"
  fi
}

ready() {
  local bin
  bin="$(find_pgbin)"
  if command -v pg_isready >/dev/null 2>&1; then pg_isready -q -h 127.0.0.1 -p "$PGPORT"; return; fi
  if [ -n "$bin" ]; then "$bin/pg_isready" -q -h 127.0.0.1 -p "$PGPORT"; return; fi
  if command -v docker >/dev/null 2>&1; then docker exec "$CONTAINER" pg_isready -q -U postgres 2>/dev/null; return; fi
  return 1
}

local_start() {
  local bin="$1"
  mkdir -p "$PGDATA" "$SOCKET_DIR"
  if [ "$(id -u)" = "0" ] && id postgres >/dev/null 2>&1; then chown -R postgres "$SOCKET_DIR"; fi
  if [ ! -f "$PGDATA/PG_VERSION" ]; then
    run_as_pg "$bin/initdb" -D "$PGDATA" -U postgres --auth=trust --no-instructions >/dev/null
  fi
  run_as_pg "$bin/pg_ctl" -D "$PGDATA" -w -l "$SOCKET_DIR/server.log" \
    -o "-p $PGPORT -k $SOCKET_DIR -c listen_addresses=127.0.0.1 -c max_connections=200 -c fsync=off -c synchronous_commit=off -c full_page_writes=off" start >/dev/null
}

docker_start() {
  if ! command -v docker >/dev/null 2>&1; then
    echo "dev-postgres: neither a local Postgres 16 (pg_ctl) nor docker was found; install one of them" >&2
    exit 1
  fi
  if docker ps -a --format '{{.Names}}' 2>/dev/null | grep -qx "$CONTAINER"; then
    docker start "$CONTAINER" >/dev/null
  else
    docker run -d --name "$CONTAINER" -e POSTGRES_USER=postgres -e POSTGRES_HOST_AUTH_METHOD=trust \
      -p "127.0.0.1:${PGPORT}:5432" postgres:16 -c fsync=off -c synchronous_commit=off -c max_connections=200 >/dev/null
  fi
  for _ in $(seq 1 60); do
    if docker exec "$CONTAINER" pg_isready -q -U postgres 2>/dev/null; then return; fi
    sleep 0.5
  done
  echo "dev-postgres: the container did not become ready" >&2
  exit 1
}

case "${1:-start}" in
  start)
    if ready 2>/dev/null; then
      echo "dev-postgres: already accepting connections on 127.0.0.1:${PGPORT}"
      echo "  KB_TEST_ADMIN_DSN=$DSN"
      exit 0
    fi
    bin="$(find_pgbin)"
    if [ -n "$bin" ]; then local_start "$bin"; else docker_start; fi
    echo "dev-postgres: ready on 127.0.0.1:${PGPORT}"
    echo "  KB_TEST_ADMIN_DSN=$DSN"
    ;;
  stop)
    bin="$(find_pgbin)"
    if [ -n "$bin" ] && [ -f "$PGDATA/postmaster.pid" ]; then
      run_as_pg "$bin/pg_ctl" -D "$PGDATA" -w stop >/dev/null && echo "dev-postgres: stopped"
    fi
    if command -v docker >/dev/null 2>&1 && docker ps --format '{{.Names}}' 2>/dev/null | grep -qx "$CONTAINER"; then
      docker stop "$CONTAINER" >/dev/null && echo "dev-postgres: container stopped"
    fi
    ;;
  status)
    if ready 2>/dev/null; then echo "dev-postgres: up on 127.0.0.1:${PGPORT} ($DSN)"; else echo "dev-postgres: down"; exit 1; fi
    ;;
  reset)
    "$0" stop || true
    rm -rf "$PGDATA"
    if command -v docker >/dev/null 2>&1; then docker rm -f "$CONTAINER" >/dev/null 2>&1 || true; fi
    exec "$0" start
    ;;
  *)
    echo "usage: $0 start|stop|status|reset" >&2
    exit 2
    ;;
esac

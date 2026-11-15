#!/usr/bin/env bash
#
# Run the query-guard ClickHouse test harness entirely in Docker:
# ClickHouse 26.8 + the query-guard proxy on a shared network.
#
# Usage:
#   scripts/docker-harness-clickhouse.sh start    # build image + start stack + smoke test
#   scripts/docker-harness-clickhouse.sh stop     # tear down
#   scripts/docker-harness-clickhouse.sh status   # show status + health
#   scripts/docker-harness-clickhouse.sh logs     # tail query-guard logs
#   scripts/docker-harness-clickhouse.sh smoke    # run the guarded-query smoke test
#
# Ports:
#   ClickHouse : 8123 (HTTP interface)
#   Proxy      : 8092
#
# ClickHouse Tier 2 is ROWS-ONLY: EXPLAIN ESTIMATE has no scan-byte or CPU
# estimate, so only max_rows limits are enforced.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
COMPOSE_FILE="$ROOT/deploy/compose/clickhouse/docker-compose.yaml"
CH="http://localhost:8123"
PROXY="http://localhost:8092"
# Credentials for the ClickHouse user bootstrapped by the compose file. They
# are sent as X-ClickHouse-User / X-ClickHouse-Key so every request exercises
# the guard's 1:1 header passthrough (the guard never decodes or logs them).
CH_AUTH=("-H" "X-ClickHouse-User: qg" "-H" "X-ClickHouse-Key: qgpass")

cmd_start() {
  echo "==> Building image (if needed) and starting stack ..."
  docker compose -f "$COMPOSE_FILE" up -d --build
  echo "==> Waiting for ClickHouse and proxy ..."
  for _ in $(seq 1 30); do
    if [ "$(curl -s -o /dev/null -w '%{http_code}' "$CH/ping")" = "200" ] &&
       [ "$(curl -s -o /dev/null -w '%{http_code}' "$PROXY/healthz")" = "200" ]; then
      break
    fi
    sleep 2
  done
  echo
  cmd_status
  echo
  cmd_smoke
}

cmd_stop() {
  echo "==> Stopping stack ..."
  docker compose -f "$COMPOSE_FILE" down
}

cmd_status() {
  echo "== containers =="
  docker compose -f "$COMPOSE_FILE" ps --format '{{.Name}}\t{{.Status}}\t{{.Ports}}'
  echo "== health =="
  echo "  proxy healthz:   $(curl -s -o /dev/null -w '%{http_code}' "$PROXY/healthz" || echo down)"
  # readyz probes the upstream /v1/info endpoint, which is Trino-specific;
  # ClickHouse has no /v1/info, so readyz reports not-ready by design.
  echo "  proxy readyz:    $(curl -s -o /dev/null -w '%{http_code}' "$PROXY/readyz" || echo down) (Trino-specific probe; expected not-ready for ClickHouse)"
  echo "  clickhouse ping: $(curl -s -o /dev/null -w '%{http_code}' "$CH/ping" || echo down)"
}

cmd_logs() {
  docker compose -f "$COMPOSE_FILE" logs -f --tail=50 query-guard
}

# ch runs a query directly against ClickHouse (bypassing the guard).
ch() { curl -s "${CH_AUTH[@]}" "$CH/" --data-binary "$1"; }

# guarded runs a query through the proxy and prints status + body.
guarded() {
  local desc="$1" query="$2"
  local bodyfile status body
  bodyfile=$(mktemp)
  status=$(curl -s -o "$bodyfile" -w '%{http_code}' "${CH_AUTH[@]}" "$PROXY/" --data-binary "$query")
  body=$(head -c 160 "$bodyfile")
  rm -f "$bodyfile"
  printf '%-28s -> %s  %s\n' "$desc" "$status" "$body"
}

cmd_smoke() {
  echo "==> Seeding tables (direct to ClickHouse) ..."
  ch "CREATE TABLE IF NOT EXISTS hits (id UInt64, name String) ENGINE = MergeTree ORDER BY id" >/dev/null
  ch "CREATE TABLE IF NOT EXISTS events (id UInt64, event_date Date) ENGINE = MergeTree ORDER BY id" >/dev/null
  ch "CREATE TABLE IF NOT EXISTS secret_data (id UInt64) ENGINE = MergeTree ORDER BY id" >/dev/null
  ch "TRUNCATE TABLE hits" >/dev/null
  # 2000 rows in hits; the policy caps default.hits at 1000 rows.
  ch "INSERT INTO hits SELECT number, toString(number) FROM numbers(2000)" >/dev/null
  ch "INSERT INTO events VALUES (1, '2026-01-01'), (2, '2026-01-02')" >/dev/null
  ch "INSERT INTO secret_data VALUES (1)" >/dev/null

  echo "==> Guarded queries through the proxy ..."
  echo "  (rows-only Tier 2: EXPLAIN ESTIMATE has no byte/CPU estimates)"
  guarded "allowed (filtered events)"   "SELECT * FROM default.events WHERE event_date = '2026-01-01'"
  guarded "blocked (hits > 1000 rows)"  "SELECT * FROM default.hits"
  guarded "blocked (table blocklist)"   "SELECT * FROM secret_data"
  guarded "blocked (required filter)"   "SELECT * FROM default.events"
}

case "${1:-}" in
  start)  cmd_start ;;
  stop)   cmd_stop ;;
  status) cmd_status ;;
  logs)   cmd_logs ;;
  smoke)  cmd_smoke ;;
  *)
    echo "Usage: $0 {start|stop|status|logs|smoke}"
    exit 1
    ;;
esac

#!/usr/bin/env bash
#
# Run the query-guard test harness entirely in Docker: Trino + the query-guard
# proxy, both as containers on a shared network. No local Java/Trino needed.
#
# Usage:
#   scripts/docker-harness.sh start    # build image + start Trino & proxy
#   scripts/docker-harness.sh stop     # stop both containers
#   scripts/docker-harness.sh status   # show status + health
#   scripts/docker-harness.sh logs     # tail query-guard logs
#
# Ports:
#   Trino  : 8082
#   Proxy  : 8091
#
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
COMPOSE_FILE="$ROOT/deploy/compose/docker-compose.yaml"

cmd_start() {
  echo "==> Building image (if needed) and starting stack ..."
  docker compose -f "$COMPOSE_FILE" up -d --build
  echo
  cmd_status
}

cmd_stop() {
  echo "==> Stopping stack ..."
  docker compose -f "$COMPOSE_FILE" down
}

cmd_status() {
  echo "== containers =="
  docker compose -f "$COMPOSE_FILE" ps --format '{{.Name}}\t{{.Status}}\t{{.Ports}}'
  echo "== health =="
  echo "  proxy healthz: $(curl -s -o /dev/null -w '%{http_code}' http://localhost:8091/healthz || echo down)"
  echo "  proxy readyz:  $(curl -s -o /dev/null -w '%{http_code}' http://localhost:8091/readyz || echo down)"
  echo "  trino:         $(curl -s -o /dev/null -w '%{http_code}' http://localhost:8082/v1/info || echo down)"
}

cmd_logs() {
  docker compose -f "$COMPOSE_FILE" logs -f --tail=50 query-guard
}

case "${1:-}" in
  start) cmd_start ;;
  stop)  cmd_stop ;;
  status) cmd_status ;;
  logs)  cmd_logs ;;
  *)
    echo "Usage: $0 {start|stop|status|logs}"
    exit 1
    ;;
esac
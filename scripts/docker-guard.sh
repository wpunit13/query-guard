#!/usr/bin/env bash
#
# Manage the query-guard Docker container (persistent, run on demand).
#
# The container runs the image built by `docker build -t query-guard:local -f
# deploy/Dockerfile .` and proxies to the host's Trino via host.docker.internal.
#
# Usage:
#   scripts/docker-guard.sh create   # (re)create the container (builds image if needed)
#   scripts/docker-guard.sh start    # start the container (creates it if missing)
#   scripts/docker-guard.sh stop     # stop the container
#   scripts/docker-guard.sh status   # show container status + health
#   scripts/docker-guard.sh logs     # tail container logs
#
# Ports:
#   Host :8091 -> container :8090
#
set -euo pipefail

NAME="qg-docker"
IMAGE="query-guard:local"
HOST_PORT="8091"
CONTAINER_PORT="8090"
TRINO_PORT=8082
POLICY_DIR="${QG_POLICY_DIR:-$HOME/.query-guard-test}"
POLICY_FILE="$POLICY_DIR/policy-docker.yaml"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"

ensure_image() {
  if ! docker image inspect "$IMAGE" >/dev/null 2>&1; then
    echo "==> Building image $IMAGE ..."
    (cd "$ROOT" && docker build -t "$IMAGE" -f deploy/Dockerfile .)
  fi
}

ensure_policy() {
  mkdir -p "$POLICY_DIR"
  if [[ -f "$POLICY_FILE" ]]; then
    return
  fi
  echo "==> Writing default docker policy to $POLICY_FILE"
  cat > "$POLICY_FILE" <<EOF
server:
  port: 8090
upstream:
  url: http://host.docker.internal:$TRINO_PORT
  timeout: 30s
preflight:
  timeout: 2s
  max_concurrent: 5
rules:
  table_blocklist:
    - lineitem
  required_filters:
    - catalog: tpch
      schema: tiny
      table: orders
      column: orderdate
  cost_limits:
    - catalog: tpch
      schema: tiny
      table: orders
      max_scan_bytes: 100000
      max_rows: 0
    - max_scan_bytes_per_query: 8000000
telemetry:
  enabled: true
  path: /metrics
EOF
}

cmd_create() {
  ensure_image
  ensure_policy
  docker rm -f "$NAME" >/dev/null 2>&1 || true
  echo "==> Creating container $NAME ($HOST_PORT -> $CONTAINER_PORT)"
  docker create --name "$NAME" \
    -p "$HOST_PORT:$CONTAINER_PORT" \
    -v "$POLICY_FILE:/etc/query-guard/policy.yaml" \
    "$IMAGE"
  echo "Created. Start with: scripts/docker-guard.sh start"
}

cmd_start() {
  if ! docker container inspect "$NAME" >/dev/null 2>&1; then
    cmd_create
  fi
  if [[ "$(docker inspect -f '{{.State.Running}}' "$NAME" 2>/dev/null)" != "true" ]]; then
    echo "==> Starting container $NAME"
    docker start "$NAME" >/dev/null
  else
    echo "Container $NAME already running."
  fi
  sleep 2
  cmd_status
}

cmd_stop() {
  docker stop "$NAME" >/dev/null 2>&1 && echo "Stopped ${NAME}." || echo "Container ${NAME} not running."
}

cmd_status() {
  echo "== $NAME =="
  if ! docker container inspect "$NAME" >/dev/null 2>&1; then
    echo "  not created"
    return
  fi
  local running
  running="$(docker inspect -f '{{.State.Running}}' "$NAME" 2>/dev/null)"
  echo "  running: $running"
  if [[ "$running" == "true" ]]; then
    echo "  healthz: $(curl -s -o /dev/null -w '%{http_code}' http://localhost:$HOST_PORT/healthz || echo down)"
    echo "  readyz:  $(curl -s -o /dev/null -w '%{http_code}' http://localhost:$HOST_PORT/readyz || echo down)"
  fi
}

cmd_logs() {
  docker logs --tail 50 "$NAME" 2>&1 || echo "Container $NAME not created."
}

case "${1:-}" in
  create) cmd_create ;;
  start)  cmd_start ;;
  stop)   cmd_stop ;;
  status) cmd_status ;;
  logs)   cmd_logs ;;
  *)
    echo "Usage: $0 {create|start|stop|status|logs}"
    exit 1
    ;;
esac
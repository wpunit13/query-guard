#!/usr/bin/env bash
#
# query-guard local test-drive harness
#
# Brings up a local Trino (latest, Java 25) plus the query-guard proxy and lets
# you exercise the guard end-to-end. Everything is self-contained under
# $BASE_DIR (default ~/.query-guard-test) and is re-created on demand, so you
# can run this whenever you want a fresh test environment.
#
# Usage:
#   scripts/test-drive.sh start    # build proxy, start Trino + proxy, wait until ready
#   scripts/test-drive.sh stop     # stop proxy + Trino
#   scripts/test-drive.sh status   # show what is running
#   scripts/test-drive.sh clean    # stop everything and delete $BASE_DIR
#
# Ports:
#   Trino  : 8082
#   Proxy  : 8090  (from policy.yaml server.port)
#
set -euo pipefail

BASE_DIR="${QG_TEST_DIR:-$HOME/.query-guard-test}"
TRINO_VERSION="476"
TRINO_DIR="$BASE_DIR/trino-server-$TRINO_VERSION"
TRINO_TARBALL="$BASE_DIR/trino-server-$TRINO_VERSION.tar.gz"
TRINO_URL="https://repo1.maven.org/maven2/io/trino/trino-server/$TRINO_VERSION/trino-server-$TRINO_VERSION.tar.gz"
TRINO_PORT=8082
TRINO_LOG="$BASE_DIR/trino.log"
PROXY_LOG="$BASE_DIR/query-guard.log"
POLICY="$(cd "$(dirname "$0")/.." && pwd)/policy.yaml"

# ──────────────────────────────────────────────────────────────────────────────
# Helpers
# ──────────────────────────────────────────────────────────────────────────────

find_java25() {
  local jh
  jh="$(/usr/libexec/java_home -v 25 2>/dev/null || true)"
  if [[ -n "$jh" ]]; then
    echo "$jh"
    return 0
  fi
  # Fallback: look for a temurin-25 install.
  local cand="$HOME/Library/Java/JavaVirtualMachines/temurin-25.jdk/Contents/Home"
  if [[ -d "$cand" ]]; then
    echo "$cand"
    return 0
  fi
  echo ""
}

ensure_trino() {
  if [[ -d "$TRINO_DIR" ]]; then
    return 0
  fi
  echo "==> Trino $TRINO_VERSION not found; downloading (~800 MB) ..."
  mkdir -p "$BASE_DIR"
  curl -fSL -o "$TRINO_TARBALL" "$TRINO_URL"
  echo "==> Extracting ..."
  tar -xzf "$TRINO_TARBALL" -C "$BASE_DIR"
  rm -f "$TRINO_TARBALL"
  configure_trino
}

configure_trino() {
  mkdir -p "$TRINO_DIR/etc/catalog"
  cat > "$TRINO_DIR/etc/config.properties" <<EOF
coordinator=true
node-scheduler.include-coordinator=true
http-server.http.port=$TRINO_PORT
query.max-memory=1GB
query.max-memory-per-node=768MB
memory.heap-headroom-per-node=256MB
discovery.uri=http://localhost:$TRINO_PORT
EOF
  cat > "$TRINO_DIR/etc/node.properties" <<EOF
node.environment=test
node.id=aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa
node.data-dir=$BASE_DIR/data
node.internal-address=127.0.0.1
EOF
  cat > "$TRINO_DIR/etc/jvm.config" <<EOF
-server
-Xmx2G
-XX:+UseG1GC
-XX:G1HeapRegionSize=32M
-XX:+ExplicitGCInvokesConcurrent
-XX:+HeapDumpOnOutOfMemoryError
EOF
  cat > "$TRINO_DIR/etc/log.properties" <<EOF
io.trino=INFO
EOF
  cat > "$TRINO_DIR/etc/catalog/tpch.properties" <<EOF
connector.name=tpch
tpch.splits-per-node=4
EOF
}

build_proxy() {
  local root
  root="$(cd "$(dirname "$0")/.." && pwd)"
  echo "==> Building query-guard binary ..."
  (cd "$root" && go build -o "$BASE_DIR/query-guard" ./cmd/queryguard)
}

wait_http() {
  local url="$1" what="$2" tries="${3:-60}"
  for _ in $(seq 1 "$tries"); do
    if curl -s -o /dev/null "$url"; then
      echo "==> $what is up ($url)"
      return 0
    fi
    sleep 1
  done
  echo "!!> $what did not become ready at $url"
  return 1
}

# ──────────────────────────────────────────────────────────────────────────────
# Commands
# ──────────────────────────────────────────────────────────────────────────────

cmd_start() {
  local java_home
  java_home="$(find_java25)"
  if [[ -z "$java_home" ]]; then
    echo "!!> Java 25 not found. Install it with:  brew install temurin@25"
    exit 1
  fi
  echo "==> Using Java 25 at: $java_home"

  ensure_trino
  build_proxy

  # Start Trino (if not already running).
  if ! curl -s -o /dev/null "http://localhost:$TRINO_PORT/v1/info"; then
    echo "==> Starting Trino $TRINO_VERSION on :$TRINO_PORT ..."
    (cd "$TRINO_DIR" && JAVA_HOME="$java_home" nohup ./bin/launcher run >"$TRINO_LOG" 2>&1 &)
  fi
  wait_http "http://localhost:$TRINO_PORT/v1/info" "Trino"

  # Start the proxy (if not already running).
  if ! curl -s -o /dev/null "http://localhost:8090/healthz"; then
    echo "==> Starting query-guard proxy on :8090 -> :$TRINO_PORT ..."
    nohup "$BASE_DIR/query-guard" -config "$POLICY" >"$PROXY_LOG" 2>&1 &
  fi
  wait_http "http://localhost:8090/healthz" "query-guard proxy"

  echo
  echo "Ready. Connect DBeaver to:  jdbc:trino://localhost:8090   (user: any, e.g. test-user)"
  echo "Direct Trino (no guard):    jdbc:trino://localhost:$TRINO_PORT"
  echo "Proxy health:               http://localhost:8090/healthz"
  echo "Proxy metrics:              http://localhost:8090/metrics"
}

cmd_stop() {
  echo "==> Stopping query-guard proxy ..."
  pkill -f "$BASE_DIR/query-guard" 2>/dev/null || true
  echo "==> Stopping Trino ..."
  pkill -f "trino-server-$TRINO_VERSION" 2>/dev/null || true
  sleep 2
  echo "Stopped."
}

cmd_status() {
  echo "== Trino ($TRINO_VERSION) on :$TRINO_PORT =="
  if curl -s -o /dev/null "http://localhost:$TRINO_PORT/v1/info"; then
    echo "  RUNNING"
  else
    echo "  stopped"
  fi
  echo "== query-guard proxy on :8090 =="
  if curl -s -o /dev/null "http://localhost:8090/healthz"; then
    echo "  RUNNING"
  else
    echo "  stopped"
  fi
}

cmd_clean() {
  cmd_stop
  echo "==> Removing $BASE_DIR ..."
  rm -rf "$BASE_DIR"
  echo "Cleaned."
}

case "${1:-}" in
  start) cmd_start ;;
  stop)  cmd_stop ;;
  status) cmd_status ;;
  clean) cmd_clean ;;
  *)
    echo "Usage: $0 {start|stop|status|clean}"
    exit 1
    ;;
esac
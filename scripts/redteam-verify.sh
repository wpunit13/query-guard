#!/usr/bin/env bash
# ──────────────────────────────────────────────────────────────────────────────
# query-guard RED-TEAM REGRESSION SUITE
#
# Fires every bypass found in the 2026-10-09 red-team exercise (see
# REDTEAM_FINDINGS.md) at the live Docker harness guards and asserts the
# expected outcome for each. Run it after any guard change:
#
#   scripts/redteam-verify.sh            # both engines
#   scripts/redteam-verify.sh trino      # trino only
#   scripts/redteam-verify.sh clickhouse # clickhouse only
#
# Exit code 0 = every expectation met. Exit 1 = N guard misses (printed).
#
# Case expectations:
#   blocked:REASON  → guard must reject with that reason
#   allowed         → guard must forward and the engine must accept the query
#                     (anti-over-blocking checks: fixes must not break legit
#                     queries)
# ──────────────────────────────────────────────────────────────────────────────
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
TRINO_GUARD="${TRINO_GUARD:-http://localhost:8091/v1/statement}"
TRINO_DIRECT="${TRINO_DIRECT:-http://localhost:8082/v1/statement}"
CH_GUARD="${CH_GUARD:-http://localhost:8092/}"
CH_DIRECT="${CH_DIRECT:-http://localhost:8123/}"
CH_USER="${CH_USER:-qg}"
CH_KEY="${CH_KEY:-qgpass}"
CH_AUTH=(-H "X-ClickHouse-User: $CH_USER" -H "X-ClickHouse-Key: $CH_KEY")

PASS=0
FAIL=0
FAILED_IDS=()

# ch_seed (re)creates the ClickHouse fixture state, idempotently.
ch_seed() {
  local q
  for q in \
    "CREATE TABLE IF NOT EXISTS default.hits (id UInt64, name String) ENGINE = MergeTree ORDER BY id" \
    "CREATE TABLE IF NOT EXISTS default.events (id UInt64, event_date Date) ENGINE = MergeTree ORDER BY id" \
    "CREATE TABLE IF NOT EXISTS default.secret_data (id UInt64) ENGINE = MergeTree ORDER BY id" \
    "CREATE VIEW IF NOT EXISTS default.v_hits AS SELECT * FROM default.hits" \
    "TRUNCATE TABLE default.hits" \
    "INSERT INTO default.hits SELECT number, toString(number) FROM numbers(2000)" \
    "TRUNCATE TABLE default.events" \
    "INSERT INTO default.events VALUES (1, '2026-01-01'), (2, '2026-01-02')" \
    "TRUNCATE TABLE default.secret_data" \
    "INSERT INTO default.secret_data VALUES (1)"
  do
    curl -s -m 30 "${CH_AUTH[@]}" "$CH_DIRECT" --data-binary "$q" >/dev/null || {
      echo "FATAL: clickhouse seeding failed (is $CH_DIRECT up?)"; exit 2
    }
  done
}

# trino_case <id> <expectation> <sql-or-@file> [curl headers...]
#   expectation: blocked:REASON | allowed
trino_case() {
  local id="$1" expect="$2" sql="$3"
  shift 3
  local bodyfile status body verdict reason
  bodyfile=$(mktemp)
  if [ "${sql#@}" != "$sql" ]; then
    status=$(curl -s -m 120 -o "$bodyfile" -w '%{http_code}' "$@" -X POST "$TRINO_GUARD" \
      -H 'X-Trino-User: redteam' --data-binary "@${sql#@}")
  else
    status=$(curl -s -m 120 -o "$bodyfile" -w '%{http_code}' "$@" -X POST "$TRINO_GUARD" \
      -H 'X-Trino-User: redteam' --data-binary "$sql")
  fi
  body=$(cat "$bodyfile"); rm -f "$bodyfile"
  if [ "$status" = "400" ] && printf '%s' "$body" | grep -q 'QUERY_GUARD_LIMIT_BREACH'; then
    reason=$(printf '%s' "$body" | sed -n 's/.*"reason":"\([^"]*\)".*/\1/p')
    case "$expect" in
      blocked:*)
        if [ "$reason" = "${expect#blocked:}" ]; then
          verdict=PASS
        else
          verdict="FAIL (blocked as $reason, want ${expect#blocked:})"
        fi
        ;;
      allowed) verdict="FAIL (unexpectedly blocked: $reason)" ;;
      *) verdict="FAIL (bad expectation '$expect')" ;;
    esac
  elif [ "$status" != "200" ]; then
    verdict="FAIL (guard/status $status: $(printf '%s' "$body" | head -c 80 | tr -d '\n'))"
  elif printf '%s' "$body" | grep -q '"error"'; then
    # forwarded, but trino rejected the query itself
    case "$expect" in
      allowed) verdict="FAIL (guard forwarded but trino errored)" ;;
      *) verdict="FAIL (guard missed; trino error: $(printf '%s' "$body" | head -c 80 | tr -d '\n'))" ;;
    esac
  else
    case "$expect" in
      allowed) verdict=PASS ;;
      blocked:*) verdict="FAIL (*** BYPASS *** — guard forwarded)" ;;
      *) verdict="FAIL (bad expectation '$expect')" ;;
    esac
  fi
  if [ "$verdict" = PASS ]; then
    PASS=$((PASS + 1)); printf '  PASS  %-13s %s\n' "$id" "$expect"
  else
    FAIL=$((FAIL + 1)); FAILED_IDS+=("$id"); printf '  FAIL  %-13s %s\n' "$id" "$verdict"
  fi
}

# ch_verdict <expectation> <status> <bodyfile>
#   Shared verdict logic for ch_case and ch_case_param.
ch_verdict() {
  local expect="$1" status="$2" bodyfile="$3"
  local body verdict reason
  body=$(cat "$bodyfile")
  if [ "$status" = "400" ] && printf '%s' "$body" | grep -q 'QUERY_GUARD_LIMIT_BREACH'; then
    reason=$(printf '%s' "$body" | sed -n 's/.*"reason":"\([^"]*\)".*/\1/p')
    case "$expect" in
      blocked:*)
        if [ "$reason" = "${expect#blocked:}" ]; then verdict=PASS
        else verdict="FAIL (blocked as $reason, want ${expect#blocked:})"; fi
        ;;
      allowed) verdict="FAIL (unexpectedly blocked: $reason)" ;;
      *) verdict="FAIL (bad expectation '$expect')" ;;
    esac
  elif [ "$status" != "200" ]; then
    verdict="FAIL (guard/status $status: $(printf '%s' "$body" | head -c 80 | tr -d '\n'))"
  elif printf '%s' "$body" | grep -qi 'exception'; then
    case "$expect" in
      allowed) verdict="FAIL (guard forwarded but clickhouse errored)" ;;
      *) verdict="FAIL (guard missed; ch error: $(printf '%s' "$body" | head -c 80 | tr -d '\n'))" ;;
    esac
  else
    # 200 with an empty body is a legitimate engine response for statements
    # that produce no output (e.g. SYSTEM FLUSH LOGS) — it still means the
    # guard forwarded the query.
    case "$expect" in
      allowed) verdict=PASS ;;
      blocked:*) verdict="FAIL (*** BYPASS *** — guard forwarded)" ;;
      *) verdict="FAIL (bad expectation '$expect')" ;;
    esac
  fi
  printf '%s' "$verdict"
}

# ch_case <id> <expectation> <sql>
ch_case() {
  local id="$1" expect="$2" sql="$3"
  local bodyfile status verdict
  bodyfile=$(mktemp)
  status=$(curl -s -m 60 -o "$bodyfile" -w '%{http_code}' "${CH_AUTH[@]}" -X POST "$CH_GUARD" --data-binary "$sql")
  verdict=$(ch_verdict "$expect" "$status" "$bodyfile")
  rm -f "$bodyfile"
  if [ "$verdict" = PASS ]; then
    PASS=$((PASS + 1)); printf '  PASS  %-13s %s\n' "$id" "$expect"
  else
    FAIL=$((FAIL + 1)); FAILED_IDS+=("$id"); printf '  FAIL  %-13s %s\n' "$id" "$verdict"
  fi
}

# ch_case_param <id> <expectation> <sql> <method>
#   Same verdicts, but the statement rides in the ?query= URL parameter
#   (ClickHouse accepts it on GET and POST) instead of the request body.
ch_case_param() {
  local id="$1" expect="$2" sql="$3" method="$4"
  local bodyfile status verdict enc
  bodyfile=$(mktemp)
  enc=$(python3 -c 'import urllib.parse, sys; print(urllib.parse.quote(sys.argv[1]))' "$sql")
  status=$(curl -s -m 60 -o "$bodyfile" -w '%{http_code}' "${CH_AUTH[@]}" -X "$method" "$CH_GUARD?query=$enc" --data-binary '')
  verdict=$(ch_verdict "$expect" "$status" "$bodyfile")
  rm -f "$bodyfile"
  if [ "$verdict" = PASS ]; then
    PASS=$((PASS + 1)); printf '  PASS  %-13s %s\n' "$id" "$expect"
  else
    FAIL=$((FAIL + 1)); FAILED_IDS+=("$id"); printf '  FAIL  %-13s %s\n' "$id" "$verdict"
  fi
}

run_trino() {
  echo "════════════════════════════════════════════════════════════"
  echo " TRINO  ($TRINO_GUARD)"
  echo "════════════════════════════════════════════════════════════"

  echo " -- baseline controls (must stay blocked) --"
  trino_case T-c1 blocked:TABLE_BLOCKLIST      "SELECT * FROM tpch.tiny.lineitem"
  trino_case T-c2 blocked:FUNCTION_BLOCKLIST   "SELECT regexp_count(name,'a') FROM tpch.tiny.nation"
  trino_case T-c3 blocked:SELECT_STAR_BLOCKED  "SELECT * FROM tpch.tiny.customer"
  trino_case T-c4 blocked:REQUIRED_FILTER_MISSING "SELECT * FROM tpch.tiny.orders"
  trino_case T-c5 blocked:COST_LIMIT_BREACH    "SELECT * FROM tpch.tiny.orders WHERE orderdate > DATE '1994-01-01'"
  trino_case T-c6 blocked:TABLE_BLOCKLIST      "TABLE tpch.tiny.lineitem"
  trino_case T-c7 blocked:TABLE_BLOCKLIST      "(SELECT * FROM tpch.tiny.lineitem)"
  trino_case T-c8 blocked:TABLE_BLOCKLIST      "PREPARE p FROM SELECT * FROM tpch.tiny.lineitem"
  trino_case T-c9 blocked:SELECT_STAR_BLOCKED  "WITH c AS (SELECT * FROM tpch.tiny.customer) SELECT name, nationkey FROM c"
  trino_case T-c10 blocked:TABLE_BLOCKLIST     "SELECT * FROM (tpch.tiny.lineitem)"

  echo " -- legit queries (must stay allowed; anti-over-blocking) --"
  trino_case T-a1 allowed "SELECT name FROM tpch.tiny.nation"
  trino_case T-a2 allowed "SELECT name FROM tpch.tiny.nation WHERE nationkey IN (SELECT orderkey FROM tpch.tiny.orders WHERE orderdate > DATE '1994-01-01')"
  trino_case T-a3 allowed "SELECT name FROM tpch.tiny.nation WHERE EXISTS (SELECT 1 FROM tpch.tiny.orders o WHERE o.orderdate > DATE '1994-01-01' AND o.nationkey = nation.nationkey)"
  trino_case T-a4 allowed "SELECT c.nationkey FROM tpch.tiny.nation, tpch.tiny.customer c"
  trino_case T-a5 allowed "SELECT name FROM tpch.tiny.nation WHERE nationkey IN (SELECT nationkey FROM tpch.tiny.customer)"
  trino_case T-a6 allowed "SELECT orderkey FROM tpch.tiny.orders WHERE orderdate > DATE '1994-01-01'"

  echo " -- red-team: EXPLAIN execution forms --"
  trino_case T-x1 blocked:TABLE_BLOCKLIST     "EXPLAIN ANALYZE SELECT * FROM tpch.tiny.lineitem"
  trino_case T-x2 blocked:TABLE_BLOCKLIST     "EXPLAIN ANALYZE VERBOSE SELECT count(*) FROM tpch.tiny.lineitem"
  trino_case T-x3 blocked:TABLE_BLOCKLIST     "EXPLAIN (TYPE EXECUTE) SELECT * FROM tpch.tiny.lineitem"
  trino_case T-x4 blocked:FUNCTION_BLOCKLIST  "EXPLAIN ANALYZE SELECT regexp_count(name,'a') FROM tpch.tiny.nation"

  echo " -- red-team: table-scanner evasion --"
  trino_case T-x5 blocked:TABLE_BLOCKLIST     "SELECT l.linenumber FROM ((tpch.tiny.lineitem)) l"
  trino_case T-x6 blocked:TABLE_BLOCKLIST     "SELECT * FROM (((tpch.tiny.lineitem)))"
  trino_case T-x7 blocked:SELECT_STAR_BLOCKED "SELECT * FROM ((tpch.tiny.customer))"
  trino_case T-x8 blocked:TABLE_BLOCKLIST     "SELECT l.linenumber FROM tpch.tiny.nation, tpch.tiny.lineitem l"
  trino_case T-x9 blocked:TABLE_BLOCKLIST     "SELECT l.linenumber FROM tpch.tiny.nation, tpch.tiny.lineitem l WHERE l.linenumber <= 2"

  echo " -- red-team: predicate-subquery evasion --"
  trino_case T-x10 blocked:SELECT_STAR_BLOCKED "SELECT name FROM tpch.tiny.nation WHERE nationkey IN (SELECT * FROM tpch.tiny.customer)"
  trino_case T-x11 blocked:FUNCTION_BLOCKLIST  "SELECT name FROM tpch.tiny.nation WHERE nationkey IN (SELECT regexp_count(name,'a') FROM tpch.tiny.nation)"
  trino_case T-x12 blocked:SELECT_STAR_BLOCKED "SELECT name FROM tpch.tiny.nation WHERE nationkey IN (WITH c AS (SELECT * FROM tpch.tiny.customer) SELECT * FROM c)"
  trino_case T-x13 blocked:SELECT_STAR_BLOCKED "SELECT name FROM tpch.tiny.nation WHERE EXISTS (SELECT * FROM tpch.tiny.customer)"

  echo " -- red-team: bare table name + session headers --"
  trino_case T-x14 blocked:REQUIRED_FILTER_MISSING "SELECT orderkey FROM orders" -H 'X-Trino-Catalog: tpch' -H 'X-Trino-Schema: tiny'
  trino_case T-x15 blocked:REQUIRED_FILTER_MISSING "SELECT orderkey, orderdate FROM orders" -H 'X-Trino-Catalog: tpch' -H 'X-Trino-Schema: tiny'

  echo " -- red-team: oversized body (server.oversized_body: reject) --"
  python3 -c "
pad = '/*' + 'a'*9437184 + '*/'
open('/tmp/qg_oversized.sql','w').write(pad + ' SELECT * FROM tpch.tiny.lineitem')
"
  trino_case T-x16 blocked:STATEMENT_TOO_LARGE "@/tmp/qg_oversized.sql"

  echo " -- red-team: crown (nested parens + preflight-timeout monster + session header) --"
  python3 -c "
arms = ' UNION ALL '.join(['SELECT %d AS x' % i for i in range(250)])
q = 'SELECT * FROM ((tpch.tiny.lineitem)) WHERE orderkey IN (SELECT x FROM (' + arms + '))'
open('/tmp/qg_crown.sql','w').write(q)
"
  trino_case T-x17 blocked:TABLE_BLOCKLIST "$(cat /tmp/qg_crown.sql)" -H 'X-Trino-Session: query_max_stage_count=10000'
}

run_clickhouse() {
  echo "════════════════════════════════════════════════════════════"
  echo " CLICKHOUSE  ($CH_GUARD)"
  echo "════════════════════════════════════════════════════════════"

  echo " -- baseline controls (must stay blocked) --"
  ch_case C-c1 blocked:TABLE_BLOCKLIST         "SELECT * FROM secret_data"
  ch_case C-c2 blocked:REQUIRED_FILTER_MISSING "SELECT * FROM default.events"
  ch_case C-c3 blocked:COST_LIMIT_BREACH       "SELECT * FROM hits"

  echo " -- legit queries (must stay allowed; anti-over-blocking) --"
  ch_case C-a1 allowed "SELECT * FROM events WHERE event_date = '2026-01-01'"
  ch_case C-a2 allowed "SELECT * FROM events PREWHERE event_date = '2026-01-01'"
  ch_case C-a3 allowed "SELECT id FROM events WHERE event_date = '2026-01-02'"

  echo " -- red-team: bare table name (default database) --"
  ch_case C-x1 blocked:REQUIRED_FILTER_MISSING "SELECT * FROM events"
  ch_case C-x2 blocked:REQUIRED_FILTER_MISSING "SELECT * FROM \`events\`"

  echo " -- red-team: FORMAT suffix vs shadow EXPLAIN ESTIMATE --"
  # count(*) plans carry no per-table estimates, so with the harness policy's
  # preflight.require_estimates the aggregate shape is rejected as
  # PREFLIGHT_NO_ESTIMATES; a plain scan shape still breaches the rows limit.
  ch_case C-x3 blocked:COST_LIMIT_BREACH "SELECT * FROM hits FORMAT JSON"
  ch_case C-x4 blocked:COST_LIMIT_BREACH "SELECT * FROM hits FORMAT CSV"
  ch_case C-x5 blocked:PREFLIGHT_NO_ESTIMATES "SELECT count(*) AS n FROM hits FORMAT JSON"

  echo " -- red-team: table functions --"
  ch_case C-x6 blocked:TABLE_FUNCTION_BLOCKLIST "SELECT * FROM merge('default','^secret')"
  ch_case C-x7 blocked:TABLE_FUNCTION_BLOCKLIST "SELECT count(*) FROM merge('default','^hits')"
  ch_case C-x8 blocked:TABLE_FUNCTION_BLOCKLIST "SELECT * FROM remote('127.0.0.1:9000', 'default', 'secret_data', '$CH_USER', '$CH_KEY')"
  ch_case C-x9 blocked:TABLE_FUNCTION_BLOCKLIST "SELECT count() FROM numbers(100000000)"

  echo " -- red-team: view with no estimates (preflight.require_estimates) --"
  ch_case C-x10 blocked:PREFLIGHT_NO_ESTIMATES "SELECT count(*) FROM v_hits"

  echo " -- red-team: bypass-class verbs --"
  ch_case C-x11 blocked:STATEMENT_BLOCKLIST "SYSTEM FLUSH LOGS"
  ch_case C-x12 blocked:STATEMENT_BLOCKLIST "KILL QUERY WHERE query_id = '00000000-0000-0000-0000-000000000000'"

  echo " -- red-team: ?query= URL parameter (GET and POST) --"
  ch_case_param C-x13 blocked:TABLE_BLOCKLIST "SELECT * FROM secret_data" POST
  ch_case_param C-x14 blocked:TABLE_BLOCKLIST "SELECT * FROM secret_data" GET
  ch_case_param C-x15 allowed "SELECT 1" GET
}

main() {
  local which="${1:-both}"
  echo
  echo "query-guard red-team regression suite — $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo

  case "$which" in
    trino) run_trino ;;
    clickhouse)
      echo "==> seeding clickhouse fixtures ..."
      ch_seed
      run_clickhouse
      ;;
    both)
      echo "==> seeding clickhouse fixtures ..."
      ch_seed
      run_trino
      echo
      run_clickhouse
      ;;
    *) echo "usage: $0 [trino|clickhouse|both]"; exit 2 ;;
  esac

  echo
  echo "────────────────────────────────────────────────────────────"
  printf 'RESULT: %d passed, %d failed\n' "$PASS" "$FAIL"
  if [ "$FAIL" -gt 0 ]; then
    printf 'failed cases: %s\n' "${FAILED_IDS[*]}"
    echo "Guard misses remain — see REDTEAM_FINDINGS.md for mechanism details."
    exit 1
  fi
  echo "All expectations met."
  exit 0
}

main "$@"

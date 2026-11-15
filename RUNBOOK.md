# query-guard Runbook

Operational guide for running query-guard in production: what the metrics
mean, which fire alerts, and what to do when they do. For setup and
architecture see `README.md`; for the backlog see `NEXT_TASKS.md`.

---

## Core operating principle

**Code defaults fail open; the shipped `policy.yaml` and Helm chart fail
closed on two explicit paths.** Parser errors and pre-flight *measurement*
failures forward the query unguarded by default (a safety tool must never
cause downtime because it could not measure). The shipped image/chart set
`preflight.fail_open: false` and `server.oversized_body: reject`, so a
pre-flight that cannot complete — or a body over the inspection cap — is
**rejected** instead of forwarded.

Consequences for operations:

- Watch the metrics (below). A guard that rejects nothing may be broken, not
  peaceful; a guard that rejects everything may be misconfigured fail-closed.
- The fail-closed knobs are explicit *policy*, not the guard's own resource
  limits. See "Measurement caps" below.

### Fail-closed opt-outs

Every fail-open path has a switch that turns it into a rejection. Parser
errors (dialect gaps) remain fail-open with no switch — a guard that cannot
parse cannot safely reject either.

| Path | Switch | Default | Opt-in behavior |
|---|---|---|---|
| Pre-flight *measurement* failure (timeout, upstream error, bad JSON, gate saturation) | `preflight.fail_open` | `true` (forward) | `false` rejects `PREFLIGHT_FAILED` |
| Plan *exceeds* `max_plan_bytes` / `max_pages` | `preflight.plan_too_large` | `forward` | `reject` rejects `PLAN_TOO_LARGE` |
| Body *exceeds* `max_statement_bytes` | `server.oversized_body` | `forward` | `reject` rejects `STATEMENT_TOO_LARGE` |
| Plan without per-table estimates (views, table functions, `count(*)`) | `preflight.require_estimates` | `false` | `true` rejects `PREFLIGHT_NO_ESTIMATES` |

The distinction that matters: **measurement failure** (couldn't reach or parse
the upstream) is governed by `fail_open`; **plan too large** (a plan that is
itself suspicious) is a policy signal governed by `plan_too_large`. The caps
are tunable (see "Measurement caps"), so an oversized plan is never a
hard-coded block — it is a decision you make.

## Engines & Tier 2 coverage

One guard process serves exactly one engine, selected by `upstream.engine`
(`trino` default, `clickhouse`, `cockroachdb`, `snowflake`). Deploy one guard
instance per engine.

**ClickHouse is rows-only in Tier 2.** `EXPLAIN ESTIMATE` reports per-table
row estimates only — there is no scan-byte or CPU estimate — so only
`max_rows` limits are enforced; `max_scan_bytes` / `max_cpu_cost_per_query`
are silently inert for ClickHouse (the same inert-limit pattern as
`queryguard_preflight_no_table_estimates_total` for Trino). Tier 1 (blocklists,
required filters, `SELECT *`, function blocklist) works identically.

Note: `/readyz` probes the upstream's `/v1/info`, a Trino protocol endpoint.
Against ClickHouse it reports not-ready (503) by design — monitor `/healthz`
and the upstream's own `/ping` instead.

## Rejection reasons

The JSON 400 body's `reason` field identifies the enforcing rule:
`TABLE_BLOCKLIST`, `REQUIRED_FILTER_MISSING` (any-of/all-of required
filters), `STATEMENT_BLOCKLIST`, `COST_LIMIT_BREACH` (scan bytes, rows, or
CPU cost), `FUNCTION_BLOCKLIST` (blocked function/UDF), `SELECT_STAR_BLOCKED`
(`SELECT *` on a restricted table), `TABLE_FUNCTION_BLOCKLIST` (table-valued
function listed in `rules.table_function_blocklist`), `STATEMENT_TOO_LARGE`
(`server.oversized_body: reject`), `PREFLIGHT_FAILED`
(`preflight.fail_open: false`), `PLAN_TOO_LARGE`
(`preflight.plan_too_large: reject`), and `PREFLIGHT_NO_ESTIMATES`
(`preflight.require_estimates: true`). The last five are fail-closed opt-outs
(see above) and never fire on a stock policy.

## Measurement caps

The pre-flight plan and the request body are read up to tunable caps so a
misbehaving upstream or a pathological query cannot exhaust the guard. Hitting
a cap is **not** a measurement failure — it is "too large", and the matching
policy knob decides what happens.

| Cap | Default | `0` means | Policy on exceed |
|---|---|---|---|
| `server.max_statement_bytes` | 8 MiB | no cap (buffer whole body) | `server.oversized_body` |
| `preflight.max_plan_bytes` | 1 MiB | unlimited | `preflight.plan_too_large` |
| `preflight.max_pages` | 64 | unlimited | `preflight.plan_too_large` |

Raise or zero these during an incident without a rebuild. Zeroing a cap
accepts its risk: `max_statement_bytes: 0` buffers the whole body (memory);
`max_plan_bytes` / `max_pages: 0` read/poll the plan without bound.

## Comprehensive policy example

```yaml
server:
  port: 8090
  read_timeout: 30s
  write_timeout: 5m
  shutdown_grace_period: 10s
  # Body inspection cap (0 = no cap). Bodies past it follow oversized_body.
  max_statement_bytes: 8388608
  # "forward" (fail-open) | "reject" (STATEMENT_TOO_LARGE)
  oversized_body: reject

upstream:
  engine: trino              # trino | clickhouse | cockroachdb | snowflake
  url: http://trino:8080
  timeout: 30s

preflight:
  timeout: 2s
  max_concurrent: 5
  # Measurement failure → forward (true) or reject (false). Code default true;
  # the shipped image/chart set false.
  fail_open: false
  # Plan read/poll caps (0 = unlimited). Exceeding them is "plan too large",
  # governed by plan_too_large — not by fail_open.
  max_plan_bytes: 1048576
  max_pages: 64
  # "forward" (fail-open) | "reject" (PLAN_TOO_LARGE)
  plan_too_large: forward
  # Reject read queries whose plan has no per-table estimates (views, table
  # functions, count(*)). Default false.
  require_estimates: true

rules:
  table_blocklist: [lineitem]
  required_filters:
    - catalog: tpch
      schema: tiny
      table: orders
      mode: any-of
      columns: [orderdate]
  cost_limits:
    - catalog: tpch
      schema: tiny
      table: orders
      max_scan_bytes: 5000000
      max_rows: 20000
    - max_scan_bytes_per_query: 8000000

audit:
  log_rejections: true
  snapshot_plan: true

telemetry:
  enabled: true
  path: /metrics
```

## Metrics reference

All metrics are exported at `GET /metrics` (path configurable via
`telemetry.path`), labeled `engine="trino"` (from the evaluator).

| Metric | Type | Meaning |
|---|---|---|
| `queryguard_queries_total{status,engine}` | counter | Queries handled, tagged `allowed` / `blocked` / `bypassed`. `bypassed` includes both intentional bypasses (SHOW/DESCRIBE/EXPLAIN/USE/SET) and fail-open forwards. Executing EXPLAIN forms (`EXPLAIN ANALYZE`, `TYPE EXECUTE`) are NOT bypasses — they are evaluated like ordinary statements. |
| `queryguard_blocked_bytes_total` | counter | Estimated scan bytes attributed to rejected queries (volume of runaway work prevented). |
| `queryguard_preflight_duration_seconds` | histogram | Tier 2 `EXPLAIN (TYPE IO)` round-trip duration. Watch P99 — this is the latency tax every SELECT pays. |
| `queryguard_parser_errors_total` | counter | SQL statements the parser could not analyze — **these were forwarded unguarded**. |
| `queryguard_preflight_rejected_total` | counter | Pre-flight evaluations turned away by the `preflight.max_concurrent` gate — forwarded unguarded (fail-open). |
| `queryguard_preflight_no_table_estimates_total` | counter | Evaluations where table-scoped cost limits could not be enforced because the plan had no per-table estimates — those limits were inert for that query. |

## Alerts

Recommended alert rules (Prometheus-style; tune thresholds to your baseline
after the pilot week):

### 1. Parser failing open — `queryguard_parser_errors_total`
- **Alert:** `increase(queryguard_parser_errors_total[15m]) > 0`
- **Meaning:** SQL dialect gaps or malformed queries. Every increment is a
  query the guard evaluated *not at all*. Sustained growth can indicate a
  deliberate bypass attempt.
- **Action:** The fail-open log line carries a `query_excerpt` of the
  statement — grep for `fail-open` with the request ID. Add the shape to a
  parser test case, and assess whether the dialect gap needs a fix or an
  explicit bypass-list entry.

### 2. Pre-flight gate saturated — `queryguard_preflight_rejected_total`
- **Alert:** `rate(queryguard_preflight_rejected_total[5m]) > 0`
- **Meaning:** More concurrent SELECTs than `preflight.max_concurrent` (default
  5). Excess queries skip the cost check and run unguarded — under a query
  storm the guard is loudest exactly when it is most needed.
- **Action:** Tune `preflight.max_concurrent` upward only after confirming the
  coordinator tolerates the EXPLAIN amplification (each pre-flight is itself a
  query). If saturation coincides with coordinator load, consider whether the
  coordinator is the bottleneck, not the guard.

### 3. Table-scoped limits inert — `queryguard_preflight_no_table_estimates_total`
- **Alert:** `increase(queryguard_preflight_no_table_estimates_total[1h]) > 0`
- **Meaning:** Plans lacking per-table estimates (aggregate-only plans such as
  `count(*)`, views, table functions, fallback paths). Table-scoped
  `max_scan_bytes`/`max_rows` were not enforced for those queries; only the
  global per-query limit applied.
- **Action:** Identify the query shape (Correlation below). If it is a common
  pattern, either add a global per-query limit that covers it, set
  `preflight.require_estimates: true` to reject such queries outright, or
  raise a bug with the plan-parse path.

### 4. Guard unreachable / readiness failing
- **Alert:** `probe_success{job="query-guard"} == 0` or `/readyz` returning 503
- **Meaning:** `/readyz` checks upstream reachability — 503 means the Trino
  coordinator is unreachable **through the guard's transport**.
- **Action:** Check upstream health directly (bypass the guard) to isolate:
  if Trino is fine but `/readyz` fails, the problem is the guard's network
  path or config (`upstream.url`). Remember: traffic does not wait for
  `/readyz` — the proxy still forwards on best effort; readiness only gates
  deployment rollouts.

### 5. Block-rate anomaly
- **Alert:** `rate(queryguard_queries_total{status="blocked"}[10m])` departs
  sharply from baseline (e.g. > 5× the 7d average)
- **Meaning:** A policy change (check `policy reloaded` logs), a new workload,
  or an abuse attempt.
- **Action:** Correlate with the config-watcher logs first — a bad hot-reload
  is the most common cause.

## Correlating a query (request ID)

Every statement request carries a **request ID**: taken from the client's
`X-Request-ID` header, or minted (16 hex chars). It appears in:

- every guard log line for that request (`request_id=…` field),
- the JSON rejection body returned to the client (`"request_id": "…"`),
- engine pre-flight log lines.

To trace a rejected or failed-open query: ask the user/tooling for the
`request_id` from the 400 response (or have clients send `X-Request-ID`),
then grep the guard's logs for it.

## Audit logging (opt-in)

With `audit.log_rejections: true`, every rejected query emits one
`audit_rejection` log line containing the verbatim query, rejection reason,
referenced tables, WHERE columns, **user** (`X-Trino-User`) and
**client_tags** (`X-Trino-Client-Tags`), and the request ID. With
`audit.snapshot_plan: true`, Tier-2 rejections additionally include the cost
snapshot (estimated bytes/rows/CPU and per-table scans).

Scope rules (see README → Security):
- Rejections only — allowed and bypassed queries are never audited.
- Identity fields appear **only** in these opt-in records, never in other logs.
- `Authorization` values are **never** logged anywhere, audit lines included.
- `log_rejections`/`snapshot_plan` are hot-reloadable.

Use the audit stream for investigations: who ran (or attempted) the blocked
query, against which tables, and (for Tier 2) how expensive it would have
been.

## CPU cost cap

`max_cpu_cost_per_query` enforces a global per-query cap using the plan's
**root** CPU estimate (`estimate.cpuCost` from the EXPLAIN I/O plan). Notes:

- It is global-only — CPU is not attributable per-table across joins.
- Many plans carry no CPU estimate; the limit then **fails open** (a warning
  is logged, the query is evaluated on the other limits only). Watch for the
  warning `max_cpu_cost_per_query configured but the plan carries no CPU cost
  estimate` — sustained growth means the CPU cap is largely inert for your
  workload.

## Config hot-reload

`policy.yaml` reloads automatically on file change (fsnotify, debounced).
On reload the guard logs `policy reloaded` with rule counts, and rebuilds the
reverse proxy if `upstream.url` changed.

- **Reload errors are safe but silent to users:** if the new YAML fails to
  parse or validate, the previous config stays active and
  `config watcher reload error` is logged. Users keep running under the old
  policy — check the logs after any intended policy change.
- **⚠️ Single-file bind mounts break hot-reload:** if `policy.yaml` is
  bind-mounted as an individual file (as in `deploy/compose`), many editors
  save via rename → the container holds the stale inode and fsnotify never
  fires; edits appear to be ignored. Restart the container after editing, or
  mount the containing **directory** instead (Kubernetes ConfigMaps already
  use symlinked directories, so the Helm deployment hot-reloads correctly).
- **Adding a rule safely:** edit a copy, validate it with the binary or a
  test load (`config.Load` rejects invalid policies at startup), then move it
  into place. Watch `queries_total{status="blocked"}` (alert #5) afterwards.

## TLS

The proxy serves HTTPS only when `server.tls.cert_file`/`key_file` are set
(see README → Security). Cert rotation is restart-based: update the mounted
Secret, then `kubectl rollout restart deployment/query-guard`. If clients
report TLS errors after a certificate renewal without a restart, that is the
first thing to check.

## Common procedures

### Guard seems to be blocking everything
1. Check for recent config reloads (`policy reloaded` log lines, mtime of the
   mounted policy).
2. Hit `/metrics`; look at `queries_total{status="blocked"}` vs `allowed`.
3. Correlate one rejected request via its `request_id`; the tier-1/tier-2 log
   line names the stage and rule that fired.
4. Roll back the policy if a recent change is implicated (hot-reload applies
   the fix immediately).

### Guard is passing everything (fail-open storm)
1. Check `parser_errors_total` and `preflight_rejected_total` first — these
   are the two fail-open paths with counters.
2. `preflight_rejected_total` growing → gate saturation (alert #2).
3. `parser_errors_total` growing → dialect gap (alert #1).
4. If neither is growing and queries are still unguarded, check whether cost
   limits are actually configured — no `cost_limits` means Tier 2 never runs
   (by design; Tier 1 still applies).
5. Mitigation: if the fail-open path is a pre-flight failure and the policy
   can tolerate rejecting instead, set `preflight.fail_open: false` (and
   `server.oversized_body: reject` for oversized bodies). Verify with
   `scripts/redteam-verify.sh` against a canary before rolling out.

### Upstream URL change (coordinator migration)
Update `upstream.url` in the mounted policy — the watcher hot-reloads and the
reverse proxy is rebuilt atomically with no dropped connections. Confirm via
`upstream updated (hot-reload)` log line and a probe request.

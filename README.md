# query-guard

A lightweight, stateless Layer 7 reverse proxy that sits in front of a database
cluster coordinator (initially Trino) and protects clusters from runaway queries
by inspecting incoming SQL at `POST /v1/statement` through a two-tier evaluation:

1. **Tier 1 (Static AST Filter):** in-memory check for table blocklists,
   function/UDF blocklists, `SELECT *` restrictions, required partition
   filters (any-of/all-of), and statement classification.
2. **Tier 2 (Pre-Flight Cost Check):** shadow execution of
   `EXPLAIN (TYPE IO, FORMAT JSON) <query>` to estimate scan bytes, rows, and
   CPU cost against configured limits.

Fail-open: if parsing, evaluation, or the pre-flight times out, the query is
forwarded unchanged so the safety tool never causes platform downtime.

---

## Policy reference

All rules are **hot-reloadable** (edit `policy.yaml`, no restart),
**case-insensitive** for unquoted identifiers, and **off unless configured**
(empty/0/false = disabled). Full example:

```yaml
server:
  port: 8090

upstream:
  url: http://trino:8080

rules:
  # Tables that may never be queried. Bare names match any catalog/schema.
  table_blocklist:
    - lineitem

  # Functions/UDFs that may not appear anywhere in a statement
  # (projections, WHERE, CTEs, subqueries).
  function_blocklist:
    - regexp_count

  # Tables on which `SELECT *` is rejected; explicit column lists are fine.
  # COUNT(*) is not a projection star and is never flagged.
  select_star_blocked_tables:
    - wide_table

  # Required WHERE filters, scoped to exactly one schema.table
  # (catalog optional; no bare-name/suffix matching).
  required_filters:
    - catalog: finance          # optional; empty = any catalog
      schema: reporting
      table: daily_orders
      mode: any-of              # any-of | all-of (default all-of)
      columns: [ds, month, year]

  cost_limits:
    # Table-scoped: scan bytes / rows for matching tables.
    - catalog: tpch
      schema: tiny
      table: orders
      max_scan_bytes: 100000
      max_rows: 0               # 0 = disabled
    # Query-global limits.
    - max_scan_bytes_per_query: 8000000
      max_cpu_cost_per_query: 500.0   # plan-root CPU estimate; 0 = disabled

# Audit logging: one structured `audit_rejection` line per rejected query
# (rejections only — allowed/bypassed queries are never audited). Identity
# fields (user, client_tags) appear ONLY here, never in other logs;
# Authorization values are never logged anywhere.
audit:
  log_rejections: true
  snapshot_plan: true   # include Tier-2 cost estimates in the audit line
```

Rejection reasons returned in the JSON 400 body: `TABLE_BLOCKLIST`,
`REQUIRED_FILTER_MISSING`, `STATEMENT_BLOCKLIST`, `COST_LIMIT_BREACH`,
`FUNCTION_BLOCKLIST`, `SELECT_STAR_BLOCKED`.

## Running the code

Build and test:

```bash
go build ./...
go test ./...
```

## Local test-drive (Trino + proxy)

Two options:

### Option A — fully Dockerized (no local Java/Trino needed)

```bash
scripts/docker-harness.sh start    # build image + start Trino & proxy in Docker
scripts/docker-harness.sh status   # show status + health
scripts/docker-harness.sh stop    # stop both containers
```

This runs both Trino (`trinodb/trino:476`) and query-guard as containers on a
shared network. Query-guard references Trino by the compose service name, so no
host networking is needed.

To change the guard policy for the Dockerized stack, edit
`deploy/compose/policy.yaml` — it is bind-mounted into the container and
hot-reloads (no restart needed for rule/upstream changes).

| Component | Address |
|-----------|---------|
| Trino (container) | `http://localhost:8082` |
| query-guard proxy (container) | `http://localhost:8091` |

### Option B — local processes (Java 25 + built binary)

`scripts/test-drive.sh` brings up a self-contained environment using your local
Java runtime:

- a **latest Trino** coordinator (requires **Java 25**, Temurin recommended)
- the **query-guard proxy** pointed at it

```bash
scripts/test-drive.sh start    # build proxy, start Trino + proxy, wait until ready
scripts/test-drive.sh stop     # stop both
scripts/test-drive.sh status   # show what is running
scripts/test-drive.sh clean    # stop everything and delete ~/.query-guard-test
```

Training wheels:
| Component | Address |
|-----------|---------|
| Trino coordinator | `http://localhost:8082` |
| query-guard proxy | `http://localhost:8090` |
| Proxy health | `http://localhost:8090/healthz` |
| Metrics | `http://localhost:8090/metrics` |

The default `policy.yaml` demonstrates all guard behaviors with the TPCH
dataset:

- **Allowed** query (required filter present, within cost limits) → `200`.
- **Tier 1** – missing required filter → `400 QUERY_GUARD_LIMIT_BREACH` /
  `REQUIRED_FILTER_MISSING`.
- **Tier 1** – `tpch.tiny.lineitem` table blocklist → `400 ... /
  TABLE_BLOCKLIST`.
- **Tier 2** – cost limit breach → `400 ... / COST_LIMIT_BREACH`.
- **Bypass** – `SHOW`/`DESCRIBE`/`EXPLAIN`/`USE`/`SET` → `200`, proxied.
- **Hot reload** – edit `policy.yaml`; the proxy picks it up at runtime without
  a restart.

Quick curl sanity check through the guard:

```bash
curl -i -X POST http://localhost:8090/v1/statement \
  -H "X-Trino-User: test-user" \
  -H "X-Trino-Catalog: tpch" \
  -H "X-Trino-Schema: tiny" \
  -d "SELECT orderkey FROM tpch.tiny.orders WHERE orderdate = DATE '1995-01-01'"
```

> Note: the proxy's **upstream URL** is fixed at startup; only the rules
> (`policy.yaml`) hot-reload. To change the upstream, restart the proxy.

---

## Connecting DBeaver

DBeaver can connect to the **proxy** (so its queries are guarded) or directly to
Trino (unguarded). The proxy mirrors Trino's standard HTTP protocol 1:1, so the
Trino JDBC driver works as usual.

### Via the proxy (guarded) — recommended

1. **Driver:** Install the `Trino` driver in DBeaver
   (Database ▸ Driver Manager ▸ New… or search "Trino" in the connection wizard;
   click "Download/Update" if prompted).
2. **New connection** → Trino:
   - **JDBC URL:** `jdbc:trino://localhost:8090`
   - **Username:** any non-empty value, e.g. `test-user`
     (Trino requires a user header; the proxy passes it through). Leave password
     blank.
   - **Catalog / Schema:** `tpch` / `tiny`
   - **Driver properties:** set `user` (or fill the username field) so the
     `X-Trino-User` header is sent.
3. **Test / connect.** Queries you run are exactly what query-guard evaluates:
   guard violations come back as a `400 QUERY_GUARD_LIMIT_BREACH` JSON response,
   which DBeaver surfaces as a query error.

Guarded connection summary:

| Field | Value |
|-------|-------|
| JDBC URL | `jdbc:trino://localhost:8090` |
| Username | `test-user` (any non-empty) |
| Catalog | `tpch` |
| Schema | `tiny` |

### Direct to Trino (unguarded/raw)

- **JDBC URL:** `jdbc:trino://localhost:8082`
- **Username:** `test-user` (any non-empty value)

### Tips

- The proxy is transparent to authentication/session headers. If you need custom
  session properties, set them as **Driver properties** (e.g.
  `X-Trino-Session` → `query_max_run_time=2m`) and they are mirrored through.
- Because Tier 1 checks run on the parsed statement, a query like
  `SELECT * FROM tpch.tiny.orders` (no `orderdate` filter) is blocked even though
  it is valid SQL on the `orders` table.
- For a quick runnable example, see the TPCH queries in `internal/integration/`.

## Health & readiness

- `GET /healthz` — liveness probe. Always `200` while the process is up.
- `GET /readyz` — readiness probe. `200` when the upstream is reachable,
  `503` otherwise. Used by the Helm chart.

## Logging & request correlation

Logs are structured (`log/slog` key-value text). Every statement request is
assigned a **request ID** — taken from an inbound `X-Request-ID` header if
present, otherwise minted — that appears in:

- all guard log lines for that request (tier 1/tier 2 decisions, fail-open
  events, pre-flight errors from the engine),
- the JSON rejection body (`"request_id": "…"`) returned to the client.

Operators: ask users for the `request_id` from a rejected response to grep
the guard's logs; send `X-Request-ID` from your own tooling to pre-correlate.

## Deployment (Kubernetes/Helm)

Build the image and lint the Helm chart:

```bash
docker build -t query-guard:local -f deploy/Dockerfile .
helm lint deploy/helm/
```

Render and inspect the manifests:

```bash
helm template query-guard deploy/helm/
helm template query-guard deploy/helm/ --set autoscaling.enabled=true
```

The chart ships:
- `templates/deployment.yaml` — probes on `/healthz` (liveness) and `/readyz`
  (readiness), policy mounted from a ConfigMap at `/etc/query-guard/policy.yaml`.
- `templates/service.yaml` — points at the proxy HTTP(S) port (`8090`).
- `templates/configmap.yaml` — mounts the `policy.yaml` from `values.policy`.
- `templates/hpa.yaml` — CPU-based autoscaling, disabled by default.

Set the `upstream.url` in `values.yaml` → `policy` to point at your Trino
coordinator service.

### Installing the chart from GHCR (OCI)

Every release tag (`v*.*.*`) publishes the chart to GHCR as an OCI artifact,
versioned to match the release:

```bash
# Pull the chart
helm pull oci://ghcr.io/<owner>/query-guard/chart --version 1.0.1

# Or install directly
helm install query-guard oci://ghcr.io/<owner>/query-guard/chart \
  --version 1.0.1 \
  --set upstream.url=http://trino:8080
```

The chart version and `appVersion` are stamped from the git tag at package
time — `Chart.yaml` in the repo is not bumped per release.

### Installing from release binaries (no Docker / Go toolchain)

Every release tag also publishes static linux binaries (amd64 + arm64) to
the GitHub Release page, packaged with a default `policy.yaml` and a
`checksums.txt`:

```bash
# Download from the release page, then verify and unpack:
sha256sum -c checksums.txt
tar xzf query-guard_<version>_linux_amd64.tar.gz
./query-guard -config policy.yaml
```

These are ideal for consumers who build their own internal images from
verified artifacts instead of pulling published container images — the
archive contains the same distroless-compatible static binary the image uses.

### TLS

Native TLS is built into the proxy. When `server.tls.cert_file` and
`server.tls.key_file` are set, the listener serves **HTTPS only** — there is
no plaintext listener, so `Authorization` and `X-Trino-*` headers are never
transmitted unencrypted. An HSTS header (`max-age=31536000; includeSubDomains`)
is sent on every response when TLS is enabled.

In the Helm chart, enable it with an existing `kubernetes.io/tls` Secret:

```bash
helm install query-guard deploy/helm/ \
  --set tls.enabled=true \
  --set tls.secretName=query-guard-tls
```

The chart mounts the Secret at `/etc/query-guard/tls` and appends the
`server.tls` block to the policy automatically. The Secret is not created by
the chart — manage it with cert-manager or your own process. Certificate
rotation is restart-based: update the Secret and restart the pods
(`kubectl rollout restart`); the distroless image has no reload signal.

## Performance

Measured with the in-repo load harness (`internal/integration/load_test.go`;
run `go test ./internal/integration/ -run TestLoad -v`). The coordinator is a
fake with a 10ms-per-hop pre-flight delay — these numbers measure the **proxy
overhead**, not real Trino EXPLAIN cost (which dominates in production and
depends entirely on your cluster).

- **Pre-flight (Tier 2) latency cost** — 200 statement submissions, 16
  concurrent clients, `preflight.max_concurrent: 5`:
  - Tier 2 OFF: P50 ≈ 2.4ms, P95 ≈ 12ms
  - Tier 2 ON (3 protocol hops × 10ms + gate queueing): P50 ≈ 38ms, P95 ≈ 50ms
  - The added latency scales with your coordinator's EXPLAIN round-trip and
    is capped by `preflight.max_concurrent` queueing. Statement bodies are
    never buffered beyond the 8 MiB inspection cap.
- **Streaming memory (flat)** — 4 × 32 MiB JSON pages through the
  Trino-URL rewrite path allocated **≈ 1 MiB** of proxy memory total
  (bounded-prefix rewrite + streamed remainder). Pages are never fully
  buffered.

## Security

**Token handling.** query-guard is a transparent passthrough: `Authorization`
and all `X-Trino-*` headers are mirrored 1:1 onto both the pre-flight request
and the proxied client request. They are never decoded, validated, logged, or
persisted. A regression test (`TestNoCredentialLogging`) asserts that
credential header values never appear in log output on any handler path.

**TLS topologies.** The proxy handles Bearer tokens, so every network leg
should be encrypted. Supported options, in order of preference:

1. **TLS on both legs (recommended).** Native TLS on the client→proxy leg
   (see TLS above) *and* an `https://` Trino upstream URL (Trino supports
   HTTPS natively via `http-server.https.enabled=true`). No plaintext anywhere.
2. **Service mesh mTLS (Istio/Linkerd).** Fully supported — the mesh encrypts
   both legs transparently; no query-guard configuration required.
3. **Ingress/LB TLS termination.** Acceptable only when the pod network is a
   trusted segment. Be aware that pod-to-pod traffic (including the Bearer
   token) is plaintext on that segment and is capturable by a compromised
   pod or node.

**Fail-open semantics.** By design, query-guard fails open: if the parser
fails, the pre-flight check times out, or an internal error occurs, the
original query is forwarded rather than dropped. A safety tool must never
cause platform downtime. The tradeoff: a crafted query that breaks the parser
bypasses the guard. This is a recorded scope decision — see `NEXT_TASKS.md`
(B6) and `progress.md` (Deferred Scope).

## Status

See `progress.md` for the phase tracker and `RUNBOOK.md` for operational
guidance (metrics, alerts, incident procedures).
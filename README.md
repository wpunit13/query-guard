# query-guard

A lightweight, stateless Layer 7 reverse proxy that sits in front of a database
cluster coordinator (initially Trino) and protects clusters from runaway queries
by inspecting incoming SQL at `POST /v1/statement` through a two-tier evaluation:

1. **Tier 1 (Static AST Filter):** in-memory check for table blocklists,
   required partition filters, and statement classification.
2. **Tier 2 (Pre-Flight Cost Check):** asynchronous shadow execution of
   `EXPLAIN (TYPE IO, FORMAT JSON) <query>` to estimate scan bytes against
   configured limits.

Fail-open: if parsing, evaluation, or the pre-flight times out, the query is
forwarded unchanged so the safety tool never causes platform downtime.

---

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
- `templates/service.yaml` — points at the proxy HTTP port (`8090`).
- `templates/configmap.yaml` — mounts the `policy.yaml` from `values.policy`.
- `templates/hpa.yaml` — CPU-based autoscaling, disabled by default.

Set the `upstream.url` in `values.yaml` → `policy` to point at your Trino
coordinator service.

## Status

See `progress.md` for the phase tracker.
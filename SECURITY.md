# Security

query-guard is a fail-open Layer 7 reverse proxy that guards Trino and
ClickHouse clusters from dangerous queries. This file states the threat model
and the limits you should understand before relying on it in production.

## What it protects

- Tier 1 (static): table, function, table-function, and statement blocklists;
  `SELECT *` restrictions; and required WHERE filters. Enforced by a
  token-based SQL analyzer.
- Tier 2 (pre-flight): per-table scan-bytes/rows and global per-query byte/CPU
  caps, via a shadow `EXPLAIN` issued to the upstream.

## What it does NOT protect

- It is not authentication. It forwards `Authorization` and engine session
  headers 1:1 to the upstream and never decides who may connect. Anyone who
  can reach the guard can attempt the guarded endpoint.
- It is not a general SQL firewall. Tier 1 is a hand-written tokenizer, not a
  grammar parser (see below).

## Known limitations

- **Tokenizer, not parser.** Tier 1 tokenizes SQL rather than parsing a
  grammar. Common constructs (nested CTEs, quoted identifiers, dialect
  clauses) are handled and covered by a red-team regression suite
  (`scripts/redteam-verify.sh`), but gaps are possible. A parse gap fails
  open: the query is forwarded unguarded. Watch
  `queryguard_parser_errors_total` — a guard that rejects nothing may be
  bypassed, not peaceful.
- **Fail-open by default.** Parser errors, pre-flight timeouts, gate
  saturation, and oversized bodies forward unguarded unless you opt out with
  `preflight.fail_open: false`, `server.oversized_body: reject`, and
  `preflight.require_estimates: true`. The shipped `policy.yaml` and Helm
  chart set the first two; apply the third where table-scoped limits are
  mandatory.
- **ClickHouse default database.** A bare table name with no
  `X-ClickHouse-Database` header or `?database=` parameter is qualified as
  `default.<table>`. If a ClickHouse user's `default_database` is customized,
  a required filter or cost limit scoped to the real schema may not match.
  Use an explicit database header/parameter, or keep the default database
  name.
- **Client-controlled session.** The shadow `EXPLAIN` runs under the client's
  own session headers (`X-Trino-Session`, catalog, schema). Estimates are only
  as trustworthy as the session the client declares.
- **Tier 2 is engine-dependent.** ClickHouse `EXPLAIN ESTIMATE` carries row
  estimates only; scan-byte and CPU limits are inert there.

## Reporting a vulnerability

Report security issues privately rather than in the public issue tracker.
Open a private security advisory, or contact a maintainer directly.

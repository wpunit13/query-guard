#!/usr/bin/env python3
"""Drive query-guard with the official Trino Python client.

This exercises the exact same wire protocol a production client (or DBeaver's
JDBC driver) uses, so it validates the guard end-to-end with no mimicry.

Prereq:  pip install trino

Usage:
    python3 scripts/qg_trino_client.py            # through the guard (8090)
    python3 scripts/qg_trino_client.py --direct   # straight to Trino (8082)
"""
import sys

from trino.dbapi import connect

GUARDED_HOST, GUARDED_PORT = "localhost", 8090
DIRECT_HOST, DIRECT_PORT = "localhost", 8082


def run(sql, use_direct=False):
    host, port = (DIRECT_HOST, DIRECT_PORT) if use_direct else (GUARDED_HOST, GUARDED_PORT)
    where = "DIRECT Trino (no guard)" if use_direct else "guard (proxy)"
    print(f"[{where} :{port}] {sql}")
    conn = connect(host=host, port=port, user="test-user", catalog="tpch", schema="tiny")
    try:
        cur = conn.cursor()
        cur.execute(sql)
        rows = cur.fetchall()
        print(f"    OK  ({len(rows)} rows)")
        for r in rows[:3]:
            print("      ", r)
        if len(rows) > 3:
            print("      ...")
    except Exception as e:  # noqa: BLE001
        print(f"    ERROR: {type(e).__name__}: {e}")
    finally:
        conn.close()
    print()


def main():
    use_direct = len(sys.argv) > 1 and sys.argv[1] == "--direct"
    gateway = "DIRECT Trino (no guard)" if use_direct else "GUARD (proxy :8090)"
    print(f"=== via {gateway} ===")
    print()

    run("SELECT name FROM tpch.tiny.customer", use_direct)                                # allowed
    run("SELECT * FROM tpch.tiny.orders", use_direct)                                     # Tier 1 block (missing filter)
    run("SELECT orderkey FROM tpch.tiny.orders WHERE orderdate = DATE '1995-01-01'",
        use_direct)                                                                       # Tier 2 block (cost breach)


if __name__ == "__main__":
    main()
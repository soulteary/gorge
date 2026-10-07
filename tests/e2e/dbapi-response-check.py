#!/usr/bin/env python3
"""Check diagnostic JSON without printing response values or credentials."""
import json
import os
import re
import sys

PUBLIC = {"user", "host", "refKey", "databaseName", "tableName", "columnName", "name", "columnType", "collation", "characterSet", "engine"}
SECRET = {"password", "passwd", "pass", "dsn", "token", "servicetoken", "secret", "query", "sql"}
SQL = re.compile(r"\b(?:SELECT\s+.+?\s+FROM|INSERT\s+INTO|UPDATE\s+.+?\s+SET|DELETE\s+FROM|INFORMATION_SCHEMA\.)", re.I)
def safe(value, password="", key=""):
    if isinstance(value, dict):
        return all(k.lower() not in SECRET and safe(v, password, k) for k, v in value.items())
    if isinstance(value, list):
        return all(safe(v, password, key) for v in value)
    if isinstance(value, str) and key not in PUBLIC:
        return not (password and password in value) and not SQL.search(value)
    return True
if __name__ == "__main__":
    try:
        ok = safe(json.load(sys.stdin), os.environ.get("DB_CHECK_PASSWORD", ""))
    except (ValueError, TypeError):
        ok = False
    sys.exit(0 if ok else 1)

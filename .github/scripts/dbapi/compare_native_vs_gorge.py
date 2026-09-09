#!/usr/bin/env python3
"""Normalized comparison of native-path vs Gorge-path database observations.

This is the assertion at the heart of the cross-repository integration test
(.github/workflows/db-api-cross-repo.yml). The workflow drives one MySQL +
Phorge + gorge-db-api stack twice against the *same* database:

  1. the native path: Phorge's own direct-SQL reflection, with `gorge.db.uri`
     unset, dumped to JSON by scripts/dbapi/dump_native.php;
  2. the Gorge path: the same Phorge reflection with `gorge.db.uri` pointed at
     the service, so PhabricatorConfigSchemaQuery / PhabricatorDatabaseRef read
     the service instead of the cluster, dumped by the same script.

Because both dumps come from Phorge's own objects, "the two paths agree" is a
statement about the whole consumer chain, not about curl output: it means the
service's wire contract, this repo's PHP adapter, and the native reflection all
describe the identical database the same way.

Stable fields (names, types, charset/collation/engine, nullability,
auto_increment, index columns/uniqueness, issue keys and fatality, applied
patches) are compared for exact equality after normalization. Volatile fields
(connection latency, replication delay, any timestamp) are compared only for
type and plausible range, because they legitimately differ run to run.

Usage:
    compare_native_vs_gorge.py NATIVE.json GORGE.json

Exit status is non-zero and a human-readable diff is printed if any stable
field disagrees, which is what fails the CI job.
"""

import json
import sys


# Fields that are allowed to differ between the two runs. They are checked for
# type/range in check_volatile() rather than equality.
VOLATILE_KEYS = {
    "connectionLatencySec",
    "secondsBehindMaster",
    "replicaDelaySec",
}


def die(msg):
    print("MISMATCH: {}".format(msg), file=sys.stderr)
    sys.exit(1)


def load(path):
    with open(path) as handle:
        return json.load(handle)


def normalize(value):
    """Drop volatile keys recursively and sort lists of dicts by a stable key.

    The native and Gorge dumps are produced by the same Phorge code, so their
    shape already matches; normalization only removes the run-to-run noise and
    makes list order irrelevant where the underlying set is what matters.
    """
    if isinstance(value, dict):
        return {
            k: normalize(v)
            for k, v in sorted(value.items())
            if k not in VOLATILE_KEYS
        }
    if isinstance(value, list):
        items = [normalize(v) for v in value]
        # Sort by a stable identity when the elements are dicts, so a different
        # server/row order does not read as a difference.
        try:
            return sorted(
                items,
                key=lambda v: json.dumps(v, sort_keys=True),
            )
        except TypeError:
            return items
    return value


def check_volatile(native, gorge, path=""):
    """Assert volatile fields are the right type and in a plausible range.

    We do not require latency or delay to match — they can not — but a
    regression that turned a number into a string, or a delay into a negative,
    would still be caught here rather than silently ignored.
    """
    if isinstance(native, dict) and isinstance(gorge, dict):
        for key in VOLATILE_KEYS:
            if key in gorge:
                v = gorge[key]
                if v is not None and not isinstance(v, (int, float)):
                    die("{}/{} is {!r}, expected a number".format(path, key, v))
                if isinstance(v, (int, float)) and v < 0:
                    die("{}/{} is negative ({})".format(path, key, v))
        for key in native:
            if key in gorge:
                check_volatile(native[key], gorge.get(key), path + "/" + key)
    elif isinstance(native, list) and isinstance(gorge, list):
        for i, item in enumerate(native):
            if i < len(gorge):
                check_volatile(item, gorge[i], "{}[{}]".format(path, i))


def main(argv):
    if len(argv) != 3:
        print(__doc__)
        return 2

    native = load(argv[1])
    gorge = load(argv[2])

    # Volatile checks run on the raw dumps (they need the volatile keys still
    # present); equality runs on the normalized copies.
    check_volatile(native, gorge)

    n = normalize(native)
    g = normalize(gorge)

    if n != g:
        # Emit a compact, greppable diff of the two normalized documents.
        n_txt = json.dumps(n, indent=2, sort_keys=True).splitlines()
        g_txt = json.dumps(g, indent=2, sort_keys=True).splitlines()
        import difflib
        diff = difflib.unified_diff(
            n_txt, g_txt, fromfile="native", tofile="gorge", lineterm="")
        print("\n".join(diff), file=sys.stderr)
        die("native and Gorge observations differ on a stable field")

    print("OK: native and Gorge observations agree on every stable field")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))

#!/usr/bin/env bash
#
# Assert the canonical contract fixtures are byte-for-byte identical in both
# repositories.
#
# The fixtures under gorge/tests/contract/dbapi/canonical/ are the source of
# truth: the Go producer generates them and the PHP consumer reads a copy. This
# script — run by the integration workflow — diffs that source against the copy
# committed in phorge-fork so the two can never drift into "same shape, wrong
# bytes" and quietly weaken the cross-repo guarantee.
#
# Usage:
#   check-contract-fixtures.sh GORGE_DIR PHORGE_DIR
#
# where GORGE_DIR is a gorge checkout and PHORGE_DIR is a phorge-fork checkout.

set -euo pipefail

gorge_dir="${1:?usage: check-contract-fixtures.sh GORGE_DIR PHORGE_DIR}"
phorge_dir="${2:?usage: check-contract-fixtures.sh GORGE_DIR PHORGE_DIR}"

src="$gorge_dir/tests/contract/dbapi/canonical"
dst="$phorge_dir/src/infrastructure/cluster/__tests__/data/gorge-contract"

status=0
for f in servers.json schema-diff.json setup-issues.json charset-info.json \
    migrations-status.json; do
  if [ ! -f "$src/$f" ]; then
    echo "::error::missing canonical source $src/$f"
    status=1
    continue
  fi
  if [ ! -f "$dst/$f" ]; then
    echo "::error::missing phorge-fork copy $dst/$f"
    status=1
    continue
  fi
  if ! diff -u "$src/$f" "$dst/$f"; then
    echo "::error::$f differs between gorge and phorge-fork"
    status=1
  fi
done

if [ "$status" -eq 0 ]; then
  echo "canonical contract fixtures are identical in both repositories"
fi

exit "$status"

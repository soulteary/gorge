#!/usr/bin/env bash
# Explicit required backend matrix; absent or broken backends fail, never skip.
# ENGINES='blob local-disk s3' BASE_URL=... TOKEN=... bash file-storage-matrix.sh
set -euo pipefail
: "${ENGINES:?Set ENGINES to the required space-separated backend identifiers}"
for engine in $ENGINES; do
  ENGINE="$engine" bash "$(dirname "${BASH_SOURCE[0]}")/file-storage.sh"
done

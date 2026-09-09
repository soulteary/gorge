#!/usr/bin/env python3
"""Static check that every service image resolves to the one canonical name.

Every gorge service ships from a single GHCR package, `ghcr.io/soulteary/gorge`,
with the service distinguished by a *tag prefix* — `render-latest`,
`db-api-2026.09.09-r1`, and so on. The one drift this guards against is the
per-service *repository* form, `ghcr.io/soulteary/gorge-<service>:<tag>`, which
looks almost identical, publishes to a package that does not exist, and is easy
to reintroduce by hand. Once one compose file uses it, `docker compose pull`
fails at deploy time with a 404 that names a tag, not the mistake.

So this check reduces every image reference to its final name and asserts three
things:

  1. The Gorge release matrix (`.github/workflows/release.yml`) and the Gorge
     deploy compose (`deploy/compose/docker-compose.yml`) name the same set of
     service prefixes against the same package.
  2. The phorge-fork overlay (`docker-compose.gorge.yml`), when present, uses
     `ghcr.io/soulteary/gorge:<prefix>-<tag>` for all ten of its gorge
     services.
  3. Nowhere in the scanned files does the `ghcr.io/soulteary/gorge-<service>`
     repository form appear.

It parses the YAML directly rather than shelling out to `docker compose config`
so it runs without Docker and without a full compose environment; where a real
`docker compose config` render is available (the workflow runs one as a
belt-and-braces step), the invariants it checks are the same. Scope is
deliberately limited to the Gorge repo and phorge-fork: the unrelated legacy
`phorge/` tree still carries the old form and is not this check's concern.

Exit status is 0 when every invariant holds and 1 otherwise, with each failure
printed on its own line.
"""

import re
import sys
from pathlib import Path

# The one package every service image must live in.
PACKAGE = "ghcr.io/soulteary/gorge"

# The per-service repository form this check exists to forbid, e.g.
# ghcr.io/soulteary/gorge-render. The tag-prefix form shares the "gorge"
# repository and separates services by tag, so the hyphen right after "gorge"
# is the tell.
FORBIDDEN_REPO_RE = re.compile(r"ghcr\.io/soulteary/gorge-[a-z0-9-]+")

# A canonical image reference looks like:
#   ghcr.io/soulteary/gorge:<prefix>-<tag...>
# The tag is variable-laden in compose (${GORGE_IMAGE_TAG:-latest},
# db-api-${GORGE_DB_IMAGE_TAG:-${GORGE_IMAGE_TAG:-latest}}), and the prefix
# itself contains hyphens (db-api, file-storage), so the split between prefix
# and tag can not be found by "first hyphen". Instead the prefix is matched
# against the known set from the release matrix: the canonical form is the
# package, a colon, one of those prefixes, a hyphen, then the tag.
def image_prefix(image, known_prefixes):
    """Return the matrix prefix a canonical image uses, or None if it uses
    none of them (which the caller reports)."""
    if not image.startswith(PACKAGE + ":"):
        return None
    tag_part = image[len(PACKAGE) + 1:]
    # Longest prefix first so "file-storage" wins over a hypothetical "file".
    for prefix in sorted(known_prefixes, key=len, reverse=True):
        if tag_part == prefix or tag_part.startswith(prefix + "-"):
            return prefix
    return None


def fail(errors, message):
    errors.append(message)


def read(path):
    return path.read_text(encoding="utf-8")


def matrix_prefixes(release_yml, errors):
    """The set of tag_prefix values in the release matrix."""
    text = read(release_yml)
    prefixes = set(re.findall(r"^\s*-?\s*tag_prefix:\s*([a-z0-9-]+)\s*$",
                              text, re.MULTILINE))
    if not prefixes:
        fail(errors, f"{release_yml}: no tag_prefix entries found in the "
                     f"release matrix")
    return prefixes


def compose_service_images(compose_yml):
    """Map every `image:` line to (service_hint, raw_image).

    The service hint is best-effort: the nearest preceding two-space-indented
    key, which in these files is the service name. It is only used for
    reporting.
    """
    images = []
    service = None
    for line in read(compose_yml).splitlines():
        m = re.match(r"^  ([a-z0-9][a-z0-9-]*):\s*$", line)
        if m:
            service = m.group(1)
            continue
        m = re.match(r"^\s*image:\s*(\S+)\s*$", line)
        if m:
            images.append((service, m.group(1)))
    return images


def prefix_of(image, known_prefixes, errors, where):
    """The service prefix of a canonical image, or None if malformed."""
    if not image.startswith(PACKAGE + ":"):
        fail(errors, f"{where}: image {image!r} is not published from the "
                     f"canonical package {PACKAGE!r}")
        return None
    prefix = image_prefix(image, known_prefixes)
    if prefix is None:
        fail(errors, f"{where}: image {image!r} does not match the canonical "
                     f"'{PACKAGE}:<prefix>-<tag>' form for any known service "
                     f"prefix")
        return None
    return prefix


def check_forbidden_form(paths, errors):
    for path in paths:
        if not path.exists():
            continue
        for lineno, line in enumerate(read(path).splitlines(), start=1):
            m = FORBIDDEN_REPO_RE.search(line)
            if m:
                fail(errors, f"{path}:{lineno}: forbidden per-service "
                             f"repository form {m.group(0)!r}; use "
                             f"'{PACKAGE}:<prefix>-<tag>' instead")


def check_gorge(gorge_root, errors):
    release_yml = gorge_root / ".github/workflows/release.yml"
    compose_yml = gorge_root / "deploy/compose/docker-compose.yml"

    prefixes = matrix_prefixes(release_yml, errors)

    compose_prefixes = set()
    for service, image in compose_service_images(compose_yml):
        prefix = prefix_of(image, prefixes, errors, f"{compose_yml} ({service})")
        if prefix is not None:
            compose_prefixes.add(prefix)

    missing_in_compose = prefixes - compose_prefixes
    missing_in_matrix = compose_prefixes - prefixes
    if missing_in_compose:
        fail(errors, f"{compose_yml}: release matrix builds "
                     f"{sorted(missing_in_compose)} but the compose file does "
                     f"not reference them")
    if missing_in_matrix:
        fail(errors, f"{compose_yml}: references service prefixes "
                     f"{sorted(missing_in_matrix)} that the release matrix "
                     f"does not build")
    return prefixes


def check_phorge_fork(phorge_root, expected_prefixes, errors):
    compose_yml = phorge_root / "docker-compose.gorge.yml"
    if not compose_yml.exists():
        print(f"note: {compose_yml} not present; skipping phorge-fork check")
        return

    found = set()
    for service, image in compose_service_images(compose_yml):
        if not image.startswith(PACKAGE):
            # Non-gorge services (mysql, redis, phorge itself) are expected and
            # not our concern.
            continue
        prefix = prefix_of(image, expected_prefixes, errors,
                           f"{compose_yml} ({service})")
        if prefix is not None:
            found.add(prefix)

    # The overlay ships exactly the ten gorge services; every prefix it uses
    # must be one the release matrix builds, and it must not invent new ones.
    unknown = found - expected_prefixes
    if unknown:
        fail(errors, f"{compose_yml}: uses gorge image prefixes {sorted(unknown)} "
                     f"that the Gorge release matrix does not build")

    expected_count = 10
    if len(found) != expected_count:
        fail(errors, f"{compose_yml}: expected {expected_count} distinct gorge "
                     f"service images, found {len(found)}: {sorted(found)}")


def main(argv):
    # Usage: check-image-names.py <gorge-root> [phorge-fork-root]
    gorge_root = Path(argv[1]) if len(argv) > 1 else Path(__file__).resolve().parents[2]
    phorge_root = Path(argv[2]) if len(argv) > 2 else gorge_root.parent / "phorge-fork"

    errors = []

    expected_prefixes = check_gorge(gorge_root, errors)
    check_phorge_fork(phorge_root, expected_prefixes, errors)
    check_forbidden_form(
        [
            gorge_root / "deploy/compose/docker-compose.yml",
            gorge_root / ".github/workflows/release.yml",
            phorge_root / "docker-compose.gorge.yml",
        ],
        errors,
    )

    if errors:
        print("Image-name check failed:")
        for e in errors:
            print(f"  - {e}")
        return 1

    print("Image-name check passed: every service resolves to "
          f"{PACKAGE}:<prefix>-<tag>.")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))

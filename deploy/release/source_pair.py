#!/usr/bin/env python3
"""Resolve a tag-owned source pair and verify its effective PHP security baseline."""
import argparse
from datetime import date
import json
from pathlib import Path
import re
import subprocess

PHP_CHECK = r"""
if (PHP_MAJOR_VERSION !== 8 || PHP_MINOR_VERSION !== 3
    || version_compare(PHP_VERSION, "8.3.36", "<")) {
  fwrite(STDERR, "PHP runtime is below the security baseline\n"); exit(1);
}
if (filter_var(ini_get("session.upload_progress.enabled"), FILTER_VALIDATE_BOOLEAN)
    || ini_get("session.use_strict_mode") !== "1") {
  fwrite(STDERR, "PHP session security baseline is not active\n"); exit(1);
}
echo "Verified PHP ".PHP_VERSION." with hardened session settings\n";
"""


def resolve_pair(lock, tag, requested=""):
    value = json.loads(Path(lock).read_text())
    if value.get("schemaVersion") != 1:
        raise ValueError("Unsupported source pair lock schema")
    match = re.fullmatch(r"(20[0-9]{2})\.([0-9]{2})\.([0-9]{2})-r([0-9]+)", tag)
    if not match:
        raise ValueError("Invalid release tag")
    date(*map(int, match.groups()[:3]))
    if value.get("release") != tag:
        raise ValueError("Release tag does not match the tag-owned source pair lock")
    sha = value.get("phorgeCommit", "")
    if not re.fullmatch(r"[0-9a-f]{40}", sha):
        raise ValueError("Source pair requires a full immutable Phorge SHA")
    if requested and requested != sha:
        raise ValueError("Requested Phorge SHA does not match the source pair lock")
    return sha


def verify_php_runtime(phorge, runner=subprocess.run):
    phorge = Path(phorge).resolve()
    dockerfile = (phorge / "Dockerfile").read_text()
    images = re.findall(r"^ARG PHP_BASE_IMAGE=(\S+)$", dockerfile, re.M)
    if len(images) != 1:
        raise ValueError("Expected exactly one pinned PHP base image")
    version = re.fullmatch(r"php:8\.3\.([0-9]+)-apache", images[0])
    if not version or int(version.group(1)) < 36:
        raise ValueError("Paired Phorge PHP image is below the security baseline")
    config = phorge / "docker/php/uploads.ini"
    if not config.is_file():
        raise ValueError("Paired Phorge session configuration is missing")
    lock = json.loads((phorge / "deploy/acceptance/build-lock.json").read_text())
    locked_php = lock.get("images", {}).get("php", "")
    if not re.fullmatch(r"docker.io/library/php@sha256:[0-9a-f]{64}", locked_php):
        raise ValueError("Paired acceptance requires an immutable official PHP image digest")
    for image in (images[0], locked_php):
        runner(["docker", "run", "--rm", "-v",
            str(config) + ":/usr/local/etc/php/conf.d/phorge-uploads.ini:ro",
            image, "php", "-r", PHP_CHECK], check=True, timeout=300)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--lock", type=Path)
    parser.add_argument("--tag")
    parser.add_argument("--requested", default="")
    parser.add_argument("--verify-php", type=Path)
    args = parser.parse_args()
    if args.verify_php:
        verify_php_runtime(args.verify_php)
    elif args.lock and args.tag:
        print(resolve_pair(args.lock, args.tag, args.requested))
    else:
        parser.error("provide --lock/--tag or --verify-php")


if __name__ == "__main__":
    main()

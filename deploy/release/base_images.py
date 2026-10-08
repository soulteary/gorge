#!/usr/bin/env python3
"""Reject platform-specific image locks before candidate builds start."""

import argparse
import json
from pathlib import Path
import re
import subprocess
import sys

INDEX_TYPES = {
    'application/vnd.oci.image.index.v1+json',
    'application/vnd.docker.distribution.manifest.list.v2+json',
}
DIGEST = re.compile(r'sha256:[0-9a-f]{64}')
IMAGE = re.compile(r'[a-z0-9][a-z0-9._/-]*@sha256:[0-9a-f]{64}')
BUILD_PLATFORMS = {'linux/amd64', 'linux/arm64'}


class BaseImageError(ValueError):
    pass


def validate_index(value, required):
    if not isinstance(value, dict) or value.get('schemaVersion') != 2 or value.get('mediaType') not in INDEX_TYPES:
        raise BaseImageError('Use a multi-platform index digest, not a platform-specific image digest.')
    descriptors = value.get('manifests')
    if not isinstance(descriptors, list) or not descriptors:
        raise BaseImageError('Image index has no platform manifests.')
    platforms = set()
    for item in descriptors:
        if not isinstance(item, dict) or not isinstance(item.get('digest'), str) or not DIGEST.fullmatch(item['digest']):
            raise BaseImageError('Image index has an invalid manifest descriptor.')
        platform = item.get('platform', {})
        if not isinstance(platform, dict):
            raise BaseImageError('Image index has an invalid platform descriptor.')
        os_name, arch = platform.get('os'), platform.get('architecture')
        # Attestation descriptors commonly use unknown/unknown. They do not
        # establish runnable support for either required architecture.
        if isinstance(os_name, str) and isinstance(arch, str) and os_name != 'unknown' and arch != 'unknown':
            platforms.add(f'{os_name}/{arch}')
    missing = required - platforms
    if missing:
        raise BaseImageError('Image index is missing required platforms: ' + ', '.join(sorted(missing)))
    return sorted(platforms)


def inspect_locked_images(lock, *, runner=subprocess.run):
    images = lock.get('images') if isinstance(lock, dict) else None
    if not isinstance(images, dict) or not {'go', 'alpine'}.issubset(images):
        raise BaseImageError('Build lock must include go and alpine images.')
    verified = {}
    for name, ref in sorted(images.items()):
        if not isinstance(name, str) or not isinstance(ref, str) or not IMAGE.fullmatch(ref):
            raise BaseImageError('Every locked image must use an immutable repository digest.')
        try:
            result = runner(['docker', 'manifest', 'inspect', ref],
                            capture_output=True, text=True, timeout=60, check=False)
        except (OSError, subprocess.TimeoutExpired) as exc:
            raise BaseImageError(f'Inspecting locked image {name} could not complete.') from exc
        if result.returncode != 0:
            raise BaseImageError(f'Inspecting locked image {name} failed (exit {result.returncode}).')
        try:
            value = json.loads(result.stdout)
            # Candidate build bases need both architectures. Full acceptance
            # runs on Ubuntu AMD64; fixture images must support that runner.
            required = BUILD_PLATFORMS if name in {'go', 'alpine'} else {'linux/amd64'}
            platforms = validate_index(value, required)
        except (ValueError, TypeError) as exc:
            raise BaseImageError(f'Locked image {name}: {exc}') from exc
        verified[name] = {'image': ref, 'platforms': platforms}
    return verified


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--lock', required=True, type=Path)
    parser.add_argument('--github-output', type=Path)
    args = parser.parse_args()
    try:
        verified = inspect_locked_images(json.loads(args.lock.read_text()))
        if args.github_output:
            with args.github_output.open('a') as output:
                output.write('go_image=' + verified['go']['image'] + '\n')
                output.write('alpine_image=' + verified['alpine']['image'] + '\n')
    except (OSError, ValueError, TypeError) as exc:
        print(f'Base image preflight failed: {exc}', file=sys.stderr)
        return 1
    print(json.dumps(verified, indent=2, sort_keys=True))
    return 0


if __name__ == '__main__':
    sys.exit(main())

#!/usr/bin/env python3
"""Bind immutable component aliases to the exact paired-accepted OCI digest."""
import argparse
import json
import os
from pathlib import Path
import re
import subprocess

from manifest import SERVICES
from registry_metadata import Registry, digest, identity, validate_index
from source_pair import resolve_pair


def accepted_image(manifest, repository, tag, sha, service):
    if service not in SERVICES:
        raise ValueError('Unknown component.')
    if manifest.get('schemaVersion') != 1 or manifest.get('release') != tag or manifest.get('gorgeCommit') != sha:
        raise ValueError('Release identity does not match paired acceptance.')
    if not re.fullmatch('[0-9a-f]{40}', manifest.get('phorgeCommit', '')):
        raise ValueError('Exact Phorge pair required.')
    images = manifest.get('images', {})
    if set(images) != set(SERVICES):
        raise ValueError('Complete accepted component set required.')
    for name, item in images.items():
        value = item.get('digest', '')
        if not re.fullmatch('sha256:[0-9a-f]{64}', value) or item.get('image') != 'ghcr.io/'+repository+'@'+value:
            raise ValueError('Accepted image must use the exact repository and digest.')
        if item.get('gorgeCommit') != sha or item.get('name') != name or item.get('port') != SERVICES[name]:
            raise ValueError('Accepted component source or packaging identity mismatch.')
    return images[service]['digest']


def inspect_component(registry, manifest, repository, tag, sha, service, require_published=False):
    expected = accepted_image(manifest, repository, tag, sha, service)
    raw = registry.manifest(expected)
    if digest(raw) != expected:
        raise ValueError('Accepted OCI index digest mismatch.')
    platforms = validate_index(raw, registry, repository, tag, sha, title='gorge-'+service)
    aliases = [service+'-'+tag, service+'-sha-'+sha]
    values = [registry.manifest(alias, missing=True) for alias in aliases]
    if any(v is None for v in values) and not all(v is None for v in values):
        raise ValueError('Partial immutable alias set requires investigation; overwrite refused.')
    exists = all(v is not None for v in values)
    if exists and any(digest(v) != expected for v in values):
        raise ValueError('Immutable alias conflicts with paired acceptance; overwrite refused.')
    if require_published and not exists:
        raise ValueError('Both immutable aliases must exist before signing.')
    return {'service': service, 'repository': repository, 'release': tag,
            'sourceRevision': sha, 'phorgeRevision': manifest['phorgeCommit'],
            'digest': expected, 'aliases': aliases, 'platforms': platforms,
            'exists': exists, 'overwritePermitted': False}


def main():
    p = argparse.ArgumentParser(description=__doc__)
    for name in ('repository', 'tag', 'sha', 'ref', 'service'):
        p.add_argument('--'+name, required=True)
    p.add_argument('--manifest', required=True, type=Path)
    p.add_argument('--output', required=True, type=Path)
    p.add_argument('--github-output', type=Path)
    p.add_argument('--require-published', action='store_true')
    a = p.parse_args()
    identity(a.repository, a.tag, a.sha, a.ref)
    if subprocess.check_output(['git', 'rev-parse', 'HEAD'], text=True).strip() != a.sha or subprocess.check_output(['git', 'rev-parse', 'refs/tags/'+a.tag+'^{commit}'], text=True).strip() != a.sha:
        raise ValueError('Checkout and release tag disagree.')
    manifest=json.loads(a.manifest.read_text())
    resolve_pair(Path(__file__).with_name('source-pair.json'),a.tag,manifest.get('phorgeCommit',''))
    registry = Registry(a.repository, os.environ.get('GHCR_USER'), os.environ.get('GHCR_TOKEN'))
    registry.authenticate()
    result = inspect_component(registry, manifest, a.repository, a.tag, a.sha, a.service, a.require_published)
    a.output.write_text(json.dumps(result, indent=2)+'\n')
    if a.github_output:
        with a.github_output.open('a') as f:
            f.write('exists='+str(result['exists']).lower()+'\ndigest='+result['digest']+'\n')
    print('Exact paired component metadata and immutable alias gate passed.')


if __name__ == '__main__':
    main()

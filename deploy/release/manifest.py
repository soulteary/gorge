#!/usr/bin/env python3
"""Bind a complete candidate set to a successful, exact paired acceptance."""
import argparse
import datetime
import hashlib
import json
from pathlib import Path
import re

SERVICES = {
    'integrations': 8210, 'maintenance': 8200, 'image': 8190, 'render': 8140,
    'notification': 22281, 'mailer': 8110, 'search': 8120, 'file-storage': 8100,
    'webhook': 8160, 'conduit': 8150, 'taskqueue': 8090, 'worker': 8170,
    'db-api': 8080, 'gitea': 8180,
}

def candidates(directory, repository, gorge_sha):
    if not re.fullmatch(r'[0-9a-f]{40}', gorge_sha):
        raise ValueError('Gorge revision must be a full commit SHA')
    items = {}
    for path in sorted(Path(directory).glob('*.json')):
        item = json.loads(path.read_text())
        name = item.get('name')
        if name not in SERVICES or name in items:
            raise ValueError('Unknown or duplicate candidate service')
        if item.get('gorgeCommit') != gorge_sha or item.get('port') != SERVICES[name]:
            raise ValueError('Candidate source or health port mismatch')
        digest = item.get('digest', '')
        if not re.fullmatch(r'sha256:[0-9a-f]{64}', digest):
            raise ValueError('Candidate requires an immutable image digest')
        items[name] = dict(item, image=repository+'@'+digest)
    if set(items) != set(SERVICES):
        raise ValueError('All fourteen service candidates are required')
    return items

def release_manifest(items, receipt, gorge_sha, phorge_sha, tag, receipt_sha):
    if not re.fullmatch(r'[0-9a-f]{40}', phorge_sha):
        raise ValueError('Phorge revision must be a full commit SHA')
    if not re.fullmatch(r'20[0-9]{2}\.[0-9]{2}\.[0-9]{2}-r[0-9]+', tag):
        raise ValueError('Release requires a versioned release tag')
    if receipt.get('schemaVersion') != 2 or receipt.get('result') != 'passed':
        raise ValueError('Paired acceptance did not pass')
    for name, sha in [('gorge', gorge_sha), ('phorge', phorge_sha)]:
        identity = receipt.get(name, {})
        if identity.get('commit') != sha or identity.get('dirty') is not False:
            raise ValueError('Acceptance must use the exact clean source pair')
        if not re.fullmatch(r'[0-9a-f]{64}', identity.get('sourceSHA256', '')):
            raise ValueError('Acceptance source inventory is missing')
    stages = receipt.get('stages', [])
    required = {'go-unit-contracts', 'paired-acceptance', 'bundled-runtime-integrity',
                'bundled-runtime-integrity-after-contracts', 'image/runtime.php'}
    if not required.issubset({s.get('name') for s in stages}):
        raise ValueError('Mandatory acceptance stages are missing')
    if any(s.get('result') != 'passed' or s.get('skippedTests') for s in stages):
        raise ValueError('Failed or skipped mandatory acceptance stages')
    expected = {name: item['image'] for name, item in items.items()}
    if receipt.get('candidateImages') != expected:
        raise ValueError('Acceptance did not bind the complete candidate image set')
    packaging = receipt.get('candidatePackaging', [])
    if len(packaging) != len(SERVICES) or {p.get('name') for p in packaging} != set(SERVICES) or any(
            p.get('result') != 'passed' for p in packaging):
        raise ValueError('All candidate images must pass packaging validation')
    # The services with a dedicated runtime fixture must run those exact images.
    fixtures = receipt.get('runtimeFixtures', [])
    if len(fixtures) != 2 or {f.get('service') for f in fixtures} != {'image','render'}:
        raise ValueError('Both actual runtime fixture containers must be inventoried')
    for fixture in fixtures:
        if expected[fixture['service']] not in fixture.get('repoDigests', []) or not re.fullmatch(
                r'sha256:[0-9a-f]{64}', fixture.get('imageId', '')) or not re.fullmatch(
                r'[0-9a-f]{64}', fixture.get('containerId', '')):
            raise ValueError('Acceptance did not run the exact candidate runtime image')
    runtime = receipt.get('runtime', {})
    if not re.fullmatch(r'[0-9a-f]{64}', runtime.get('sourceSHA256', '')):
        raise ValueError('Bundled runtime inventory is missing')
    return {'schemaVersion': 1, 'release': tag,
            'createdAt': datetime.datetime.now(datetime.timezone.utc).isoformat(),
            'gorgeCommit': gorge_sha, 'phorgeCommit': phorge_sha,
            'runtime': runtime, 'images': items,
            'acceptance': {'sha256': receipt_sha, 'scope': 'paired real-backend contracts',
                           'notCovered': receipt.get('notCovered', [])}}

def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--candidates', required=True, type=Path)
    p.add_argument('--repository', required=True)
    p.add_argument('--gorge-sha', required=True)
    p.add_argument('--phorge-sha')
    p.add_argument('--tag')
    p.add_argument('--acceptance', type=Path)
    p.add_argument('--output', required=True, type=Path)
    args = p.parse_args()
    items = candidates(args.candidates, args.repository, args.gorge_sha)
    if args.acceptance:
        raw = args.acceptance.read_bytes()
        value = release_manifest(items, json.loads(raw), args.gorge_sha, args.phorge_sha,
                                 args.tag, hashlib.sha256(raw).hexdigest())
    else:
        value = {name: item['image'] for name, item in items.items()}
    args.output.write_text(json.dumps(value, indent=2, sort_keys=True)+'\n')

if __name__ == '__main__':
    main()

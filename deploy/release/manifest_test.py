import copy
import json
from pathlib import Path
import tempfile
import unittest

from manifest import SERVICES, candidates, release_manifest
from packaging_check import validate_packaging

class ReleaseGate(unittest.TestCase):
    def fixtures(self):
        sha = 'a'*40
        items = {name: {'name': name, 'port': port, 'gorgeCommit': sha,
                       'digest': 'sha256:'+'b'*64, 'image': 'ghcr.io/example/gorge@sha256:'+'b'*64}
                 for name, port in SERVICES.items()}
        receipt = {'schemaVersion': 2, 'result': 'passed',
                   'gorge': {'commit': sha, 'dirty': False, 'sourceSHA256': 'c'*64},
                   'phorge': {'commit': 'd'*40, 'dirty': False, 'sourceSHA256': 'e'*64},
                   'runtime': {'sourceSHA256': 'f'*64},
                   'stages': [{'name': n, 'result': 'passed', 'skippedTests': []} for n in
                              ['go-unit-contracts','paired-acceptance','bundled-runtime-integrity',
                               'bundled-runtime-integrity-after-contracts','image/runtime.php']],
                   'candidateImages': {n: item['image'] for n,item in items.items()},
                   'candidatePackaging': [{'name': n, 'result': 'passed'} for n in items],
                   'images': [{'repoDigests': [items['image']['image']]}],
                   'runtimeFixtures': [{'service':n,'containerId':'1'*64,'imageId':'sha256:'+'2'*64,
                                        'repoDigests':[items[n]['image']]} for n in ('image','render')]}
        return items, receipt

    def build(self, items, receipt):
        return release_manifest(items, receipt, 'a'*40, 'd'*40, '2026.10.08-r1', '1'*64)

    def test_complete_pair_passes(self):
        items, receipt = self.fixtures()
        self.assertEqual(set(self.build(items,receipt)['images']), set(SERVICES))

    def test_failed_dirty_wrong_source_skipped_or_unrun_candidates_refused(self):
        items, receipt = self.fixtures()
        mutations = [lambda r: r.update(result='failed'),
                     lambda r: r['phorge'].update(dirty=True),
                     lambda r: r['gorge'].update(commit='2'*40),
                     lambda r: r['stages'][0].update(skippedTests=[{'test':'real-database'}]),
                     lambda r: r['stages'].pop(),
                     lambda r: r['candidateImages'].pop('worker'),
                     lambda r: r['candidatePackaging'].pop(),
                     lambda r: r.update(runtimeFixtures=[]),
                     lambda r: r['runtimeFixtures'][0].update(repoDigests=['other@sha256:'+'b'*64]),
                     lambda r: r.update(runtime={})]
        for mutate in mutations:
            changed = copy.deepcopy(receipt); mutate(changed)
            with self.subTest(mutation=mutate), self.assertRaises(ValueError):
                self.build(items,changed)

    def test_missing_duplicate_bad_digest_and_wrong_health_port_refused(self):
        items, _ = self.fixtures()
        with tempfile.TemporaryDirectory() as tmp:
            directory = Path(tmp)
            for name, item in items.items():
                (directory/(name+'.json')).write_text(json.dumps(item))
            self.assertEqual(len(candidates(directory, 'ghcr.io/example/gorge', 'a'*40)),14)
            (directory/'extra.json').write_text(json.dumps(items['worker']))
            with self.assertRaises(ValueError): candidates(directory,'ghcr.io/example/gorge','a'*40)
            (directory/'extra.json').unlink()
            for mutate in [lambda i: i.update(digest='worker-latest'), lambda i: i.update(port=8140),
                           lambda i: i.update(gorgeCommit='3'*40)]:
                item = dict(items['worker']); mutate(item)
                (directory/'worker.json').write_text(json.dumps(item))
                with self.assertRaises(ValueError): candidates(directory,'ghcr.io/example/gorge','a'*40)
            (directory/'worker.json').unlink()
            with self.assertRaises(ValueError): candidates(directory,'ghcr.io/example/gorge','a'*40)

    def test_packaging_rejects_incorrect_ports_root_and_other_source(self):
        info = {'Os':'linux','Config':{'User':'gorge','Entrypoint':['gorge-service'],
                'Labels':{'org.opencontainers.image.revision':'a'*40},
                'Env':['GORGE_HEALTHCHECK_PORT=8170'], 'ExposedPorts':{'8170/tcp':{}},
                'Healthcheck':{'Test':['CMD-SHELL','wget $GORGE_HEALTHCHECK_PORT/healthz']}}}
        validate_packaging(info,'worker','a'*40)
        for mutate in [lambda c: c.update(User='root'), lambda c: c.update(Env=['GORGE_HEALTHCHECK_PORT=8140']),
                       lambda c: c.update(Healthcheck={}), lambda c: c['Labels'].clear()]:
            changed=copy.deepcopy(info); mutate(changed['Config'])
            with self.assertRaises(ValueError): validate_packaging(changed,'worker','a'*40)

if __name__ == '__main__': unittest.main()

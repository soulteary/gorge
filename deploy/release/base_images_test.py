import copy
import json
import subprocess
import unittest

from base_images import BaseImageError, BUILD_PLATFORMS, INDEX_TYPES, inspect_locked_images, validate_index


def index(platforms=('amd64', 'arm64'), media_type='application/vnd.oci.image.index.v1+json'):
    return {'schemaVersion': 2, 'mediaType': media_type,
            'manifests': [{'digest': 'sha256:' + 'a'*64,
                           'platform': {'os': 'linux', 'architecture': arch}}
                          for arch in platforms]}


class LockedImagePlatforms(unittest.TestCase):
    def test_both_index_formats_accept_required_platforms_and_ignore_attestations(self):
        for media_type in INDEX_TYPES:
            value = index(media_type=media_type)
            value['manifests'].append({'digest': 'sha256:' + 'b'*64,
                                      'platform': {'os': 'unknown', 'architecture': 'unknown'}})
            self.assertEqual(validate_index(value, BUILD_PLATFORMS), ['linux/amd64', 'linux/arm64'])

    def test_arm64_child_manifest_from_failed_release_is_rejected(self):
        value = {'schemaVersion': 2, 'mediaType': 'application/vnd.oci.image.manifest.v1+json',
                 'config': {'digest': 'sha256:' + 'a'*64}, 'layers': []}
        with self.assertRaisesRegex(BaseImageError, 'platform-specific'):
            validate_index(value, BUILD_PLATFORMS)

    def test_one_architecture_or_attestation_cannot_pass_multiarch_gate(self):
        for platforms in [('arm64',), ('amd64',), ('unknown',)]:
            with self.subTest(platforms=platforms), self.assertRaisesRegex(BaseImageError, 'missing required'):
                validate_index(index(platforms), BUILD_PLATFORMS)

    def test_malformed_index_rejected(self):
        for mutation in [lambda v: v.update(manifests=[]),
                         lambda v: v['manifests'][0].update(digest='latest'),
                         lambda v: v['manifests'][0].update(platform=None)]:
            value = copy.deepcopy(index())
            mutation(value)
            with self.assertRaises(BaseImageError):
                validate_index(value, BUILD_PLATFORMS)

    def test_fixture_must_support_acceptance_runner(self):
        self.assertEqual(validate_index(index(('amd64',)), {'linux/amd64'}), ['linux/amd64'])
        with self.assertRaises(BaseImageError):
            validate_index(index(('arm64',)), {'linux/amd64'})

    def test_registry_inspection_uses_exact_digests_and_fails_closed(self):
        ref = 'docker.io/library/example@sha256:' + 'c'*64
        lock = {'images': {'go': ref, 'alpine': ref, 'mysql': ref}}
        calls = []

        def runner(args, **kwargs):
            calls.append((args, kwargs))
            return subprocess.CompletedProcess(args, 0, json.dumps(index()), '')

        self.assertEqual(set(inspect_locked_images(lock, runner=runner)), {'go', 'alpine', 'mysql'})
        self.assertEqual(calls[0][0], ['docker', 'manifest', 'inspect', ref])
        self.assertEqual(calls[0][1]['timeout'], 60)
        failures = [subprocess.CompletedProcess([], 1, '', 'credential-bearing error'),
                    subprocess.CompletedProcess([], 0, '{bad json', ''),
                    subprocess.TimeoutExpired('docker', 60)]
        for failure in failures:
            def failed(*args, **kwargs):
                if isinstance(failure, Exception):
                    raise failure
                return failure
            with self.assertRaises(BaseImageError):
                inspect_locked_images(lock, runner=failed)

    def test_mutable_or_missing_base_refs_rejected_before_registry_call(self):
        for lock in [{'images': {'go': 'golang:latest', 'alpine': 'alpine:latest'}}, {'images': {}}]:
            with self.assertRaises(BaseImageError):
                inspect_locked_images(lock, runner=lambda *args, **kwargs: self.fail('Unexpected registry call'))


if __name__ == '__main__':
    unittest.main()

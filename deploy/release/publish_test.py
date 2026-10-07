import json
from pathlib import Path
import re
import subprocess
import tempfile
import unittest

from publish import PublishError, included_response, publish, release_version


def response(status, body=None, returncode=None, newline='\n'):
    if returncode is None:
        returncode = 0 if status == 200 else 1
    text = f'HTTP/2.0 {status} Response{newline}Content-Type: application/json{newline}{newline}'
    text += json.dumps(body if body is not None else {'message': 'Not Found'})
    return subprocess.CompletedProcess([], returncode, text, '')


class FakeGitHub:
    """No subprocess or network: a shared latest pointer models serialized jobs."""
    def __init__(self, latest=None):
        self.latest = latest
        self.releases = {} if latest is None else {latest: False}
        self.calls = []
        self.injected = {}

    def __call__(self, args, **kwargs):
        self.calls.append((args, kwargs))
        index = len(self.calls)
        if index in self.injected:
            value = self.injected[index]
            if isinstance(value, Exception):
                raise value
            return value
        if args[:2] == ['gh', 'api']:
            endpoint = args[-1]
            if endpoint.endswith('/latest'):
                return response(404) if self.latest is None else response(200, {'tag_name': self.latest})
            tag = endpoint.split('/tags/')[1]
            return response(200, {'tag_name': tag}) if tag in self.releases else response(404)
        operation, tag = args[2:4]
        if operation == 'create':
            if '--draft' not in args or '--latest=false' not in args:
                raise AssertionError('Draft creation must suppress automatic latest selection.')
            self.releases[tag] = True
        elif operation == 'edit':
            if '--draft=false' in args:
                if '--latest=false' not in args:
                    raise AssertionError('Publishing must suppress automatic latest selection.')
                self.releases[tag] = False
            if '--latest' in args:
                if self.releases[tag]:
                    raise AssertionError('Latest must only point at a published release.')
                self.latest = tag
        else:
            raise AssertionError(f'Unexpected fake operation: {operation}')
        return subprocess.CompletedProcess(args, 0, 'release-url', '')


class ReleasePublication(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        directory = Path(self.temporary.name)
        self.manifest = directory / 'release-manifest.json'
        self.notes = directory / 'notes.txt'
        self.manifest.write_text('{"verified": true}')
        self.notes.write_text('Use the verified manifest.')

    def run_publish(self, fake, tag='2026.10.08-r10', **kwargs):
        return publish('example/gorge', tag, self.manifest, self.notes, runner=fake, **kwargs)

    def assert_no_mutations(self, fake):
        self.assertTrue(all(args[1] == 'api' for args, _ in fake.calls))

    def test_older_version_finishing_after_newer_keeps_latest(self):
        fake = FakeGitHub('2026.10.07-r1')
        self.assertTrue(self.run_publish(fake, '2026.10.09-r1'))
        self.assertFalse(self.run_publish(fake, '2026.10.08-r99'))
        self.assertEqual(fake.latest, '2026.10.09-r1')
        self.assertFalse(fake.releases['2026.10.08-r99'])
        self.assertEqual(sum('--latest' in args for args, _ in fake.calls), 1)
        # The later-finishing job reads the advanced pointer inside the lock.
        self.assertEqual(fake.calls[5][0][-1], 'repos/example/gorge/releases/latest')

    def test_integer_revision_and_calendar_date_order(self):
        for previous, new, promoted in [
                ('2026.10.08-r9', '2026.10.08-r10', True),
                ('2026.10.08-r10', '2026.10.08-r9', False),
                ('2026.10.08-r999', '2026.10.09-r0', True),
                ('2026.10.09-r0', '2026.10.08-r999', False),
                ('2026.09.30-r9', '2026.10.01-r1', True),
                ('2025.12.31-r999', '2026.01.01-r1', True),
                ('2026.10.08-r09', '2026.10.08-r9', False)]:
            with self.subTest(previous=previous, new=new):
                fake = FakeGitHub(previous)
                self.assertEqual(self.run_publish(fake, new), promoted)
                self.assertEqual(fake.latest, new if promoted else previous)

    def test_explicit_404_is_the_only_absent_latest(self):
        for returncode in (0, 1):
            with self.subTest(returncode=returncode):
                fake = FakeGitHub()
                fake.injected[1] = response(404, returncode=returncode)
                self.assertTrue(self.run_publish(fake))
                self.assertEqual(fake.latest, '2026.10.08-r10')

    def test_reads_before_writes_and_publishes_before_promotion(self):
        fake = FakeGitHub('2026.10.08-r9')
        self.run_publish(fake, timeout=7)
        self.assertEqual(fake.calls[0][0], ['gh', 'api', '--include', '--method', 'GET',
                                          'repos/example/gorge/releases/latest'])
        self.assertEqual(fake.calls[1][0][-1], 'repos/example/gorge/releases/tags/2026.10.08-r10')
        self.assertEqual(fake.calls[2][0][1:4], ['release', 'create', '2026.10.08-r10'])
        self.assertIn('--verify-tag', fake.calls[2][0])
        self.assertEqual(fake.calls[3][0], ['gh', 'release', 'edit', '2026.10.08-r10',
                                          '--draft=false', '--latest=false', '--repo', 'example/gorge'])
        self.assertEqual(fake.calls[4][0], ['gh', 'release', 'edit', '2026.10.08-r10',
                                          '--latest', '--repo', 'example/gorge'])
        for _, kwargs in fake.calls:
            self.assertEqual(kwargs, dict(capture_output=True, text=True, timeout=7, check=False))

    def test_non_404_errors_and_nonzero_200_stop_before_writes(self):
        for status, returncode in [(401, 1), (403, 1), (429, 1), (500, 1),
                                   (503, 0), (200, 1), (404, 2)]:
            for index in (1, 2):
                with self.subTest(status=status, returncode=returncode, index=index):
                    fake = FakeGitHub('2026.10.08-r9')
                    fake.injected[index] = response(status, {'tag_name': '2026.10.08-r9'}, returncode)
                    with self.assertRaises(PublishError):
                        self.run_publish(fake)
                    self.assert_no_mutations(fake)

    def test_missing_ambiguous_and_malformed_headers_stop_before_writes(self):
        for output in [
                '{"message":"Not Found"}',
                'gh: Not Found (HTTP 404)',
                'HTTP/2.0 404 Not Found\nContent-Type: json\n',
                'HTTP/2.0 404 Not Found\nNot a header\n\n{}',
                'HTTP/2.0 404 Not Found\n\nHTTP/2.0 200 OK\n\n{}',
                'HTTP/2.0 200 OK\n\nHTTP/2.0 404 Not Found\n\n{}',
                'HTTP/2.0 200 OK\n\nHTTP/2.0 200 OK\n\n{"tag_name":"2026.10.08-r9"}']:
            with self.subTest(output=output):
                fake = FakeGitHub()
                fake.injected[1] = subprocess.CompletedProcess([], 1, output, 'untrusted diagnostic')
                with self.assertRaises(PublishError):
                    self.run_publish(fake)
                self.assert_no_mutations(fake)

    def test_bad_json_or_missing_release_tag_stops_before_writes(self):
        for body in ['{broken', 'null', '[]', '{}', '{"tag_name":4}',
                     '{"tag_name":"2026.10.08-r9"}\n{"tag_name":"2026.10.08-r10"}']:
            with self.subTest(body=body):
                fake = FakeGitHub()
                fake.injected[1] = subprocess.CompletedProcess([], 0, 'HTTP/2.0 200 OK\n\n' + body, '')
                with self.assertRaises(PublishError):
                    self.run_publish(fake)
                self.assert_no_mutations(fake)

    def test_404_with_malformed_response_body_is_not_absence(self):
        for body in ('', '{broken', 'null', '[]', 'HTTP/2.0 200 OK\n\n{}'):
            with self.subTest(body=body):
                fake = FakeGitHub()
                fake.injected[1] = subprocess.CompletedProcess([], 1, 'HTTP/2.0 404 Not Found\n\n' + body, '')
                with self.assertRaises(PublishError):
                    self.run_publish(fake)
                self.assert_no_mutations(fake)

    def test_unknown_or_invalid_latest_version_stops_before_writes(self):
        for previous in ['v2026.10.08-r9', 'v1.0.0', 'latest', '2026.02.29-r1',
                         '2026.13.01-r1', '2026.10.08-r-1', '2026.10.08-r1suffix']:
            with self.subTest(previous=previous):
                fake = FakeGitHub(previous)
                with self.assertRaises(PublishError):
                    self.run_publish(fake)
                self.assertEqual(len(fake.calls), 1)
                self.assert_no_mutations(fake)

    def test_existing_release_is_not_overwritten(self):
        fake = FakeGitHub('2026.10.08-r10')
        with self.assertRaisesRegex(PublishError, 'already exists'):
            self.run_publish(fake)
        self.assert_no_mutations(fake)

    def test_invalid_new_version_and_missing_inputs_do_not_call_gh(self):
        fake = FakeGitHub()
        for new in ['2026.02.29-r1', '2026.00.01-r1', 'v2026.10.08-r1', '2026.10.08-r1\n']:
            with self.subTest(new=new), self.assertRaises(PublishError):
                self.run_publish(fake, new)
        self.notes.write_text('')
        with self.assertRaises(PublishError):
            self.run_publish(fake)
        self.notes.unlink()
        with self.assertRaises(PublishError):
            self.run_publish(fake)
        self.assertEqual(fake.calls, [])

    def test_every_mutation_failure_stops_later_commands(self):
        for index in (3, 4, 5):
            with self.subTest(index=index):
                fake = FakeGitHub()
                fake.injected[index] = subprocess.CompletedProcess([], 1, 'secret response', 'secret error')
                with self.assertRaises(PublishError) as caught:
                    self.run_publish(fake)
                self.assertNotIn('secret', str(caught.exception))
                self.assertEqual(len(fake.calls), index)
                self.assertIsNone(fake.latest)

    def test_every_command_timeout_stops_later_commands(self):
        for index in range(1, 6):
            with self.subTest(index=index):
                fake = FakeGitHub()
                fake.injected[index] = subprocess.TimeoutExpired('gh', 30, output='secret')
                with self.assertRaisesRegex(PublishError, 'timed out') as caught:
                    self.run_publish(fake)
                self.assertNotIn('secret', str(caught.exception))
                self.assertEqual(len(fake.calls), index)

    def test_command_start_failure_and_invalid_configuration(self):
        fake = FakeGitHub()
        fake.injected[1] = OSError('secret diagnostic')
        with self.assertRaisesRegex(PublishError, 'could not start'):
            self.run_publish(fake)
        for timeout in (0, 121):
            with self.subTest(timeout=timeout), self.assertRaises(PublishError):
                self.run_publish(FakeGitHub(), timeout=timeout)
        with self.assertRaises(PublishError):
            publish('example/gorge/extra', '2026.10.08-r1', self.manifest, self.notes, runner=FakeGitHub())

    def test_parser_accepts_cli_protocol_versions_and_crlf(self):
        for protocol in ('HTTP/1.1', 'HTTP/2', 'HTTP/2.0', 'HTTP/3.0'):
            output = f'{protocol} 200 OK\r\nContent-Type: application/json\r\n\r\n{{}}'
            self.assertEqual(included_response(output), (200, '{}'))
        self.assertLess(release_version('2024.02.29-r9'), release_version('2024.02.29-r10'))

    def test_workflow_holds_global_lock_around_helper(self):
        workflow = (Path(__file__).parents[2] / '.github/workflows/release.yml').read_text()
        publish_job = workflow.split('\n  publish:\n', 1)[1]
        self.assertRegex(publish_job, re.compile(
            r'    concurrency:\n      group: gorge-release-publication\n      cancel-in-progress: false\n'))
        self.assertIn('python3 deploy/release/publish.py --repository "$GITHUB_REPOSITORY"', publish_job)
        self.assertNotIn('gh release ', publish_job)


if __name__ == '__main__':
    unittest.main()

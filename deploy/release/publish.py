#!/usr/bin/env python3
"""Publish a verified release while the workflow holds its repository-wide lock."""

import argparse
from datetime import date
import json
from pathlib import Path
import re
import subprocess
import sys


class PublishError(ValueError):
    pass


def release_version(tag):
    match = re.fullmatch(r'(20[0-9]{2})\.([0-9]{2})\.([0-9]{2})-r([0-9]+)', tag)
    if not match:
        raise PublishError('Release tag must be YYYY.MM.DD-rN.')
    try:
        year, month, day, revision = map(int, match.groups())
        return date(year, month, day), revision
    except ValueError as exc:
        raise PublishError('Release tag has an invalid calendar date or revision.') from exc


def included_response(output):
    """Only accept one explicit HTTP status/header block from `gh api --include`."""
    output = output.replace('\r\n', '\n')
    head, separator, body = output.partition('\n\n')
    lines = head.split('\n')
    status = re.fullmatch(r'HTTP/[0-9]+(?:\.[0-9]+)? ([0-9]{3})(?: [^\r\n]*)?', lines[0])
    if not separator or not status or any(
            not re.fullmatch(r"[!#$%&'*+.^_`|~0-9A-Za-z-]+:[^\r\n]*", line)
            for line in lines[1:]):
        raise PublishError('GitHub API did not return an unambiguous HTTP status.')
    return int(status.group(1)), body


class GitHub:
    def __init__(self, repository, timeout=30, runner=subprocess.run):
        if not re.fullmatch(r'[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+', repository):
            raise PublishError('Repository must be an explicit owner/name.')
        if not 1 <= timeout <= 120:
            raise PublishError('Command timeout must be between 1 and 120 seconds.')
        self.repository = repository
        self.timeout = timeout
        self.runner = runner

    def command(self, arguments, operation):
        try:
            return self.runner(['gh', *arguments], capture_output=True, text=True,
                               timeout=self.timeout, check=False)
        except subprocess.TimeoutExpired as exc:
            # Never print raw CLI output: it may contain credentials or response bodies.
            raise PublishError(f'{operation} timed out; its outcome must be checked.') from exc
        except OSError as exc:
            raise PublishError(f'{operation} could not start.') from exc

    def read_release(self, suffix):
        result = self.command(['api', '--include', '--method', 'GET',
                               f'repos/{self.repository}/releases/{suffix}'],
                              'Reading release state')
        status, body = included_response(result.stdout)
        # `gh api` normally exits 1 for an HTTP 404. No other failed command
        # or missing/malformed status is evidence that a release does not exist.
        absent = status == 404 and result.returncode in (0, 1)
        if not absent and (result.returncode != 0 or status != 200):
            raise PublishError(f'Reading release state failed (HTTP {status}, exit {result.returncode}).')
        try:
            value = json.loads(body)
        except (ValueError, TypeError) as exc:
            raise PublishError('GitHub API returned invalid release JSON.') from exc
        if not isinstance(value, dict):
            raise PublishError('GitHub API did not return a release response object.')
        if absent:
            return None
        if not isinstance(value.get('tag_name'), str):
            raise PublishError('GitHub API did not return a release tag.')
        return value

    def mutate(self, arguments, operation):
        result = self.command(['release', *arguments, '--repo', self.repository], operation)
        if result.returncode != 0:
            raise PublishError(f'{operation} failed (exit {result.returncode}); its outcome must be checked.')


def publish(repository, tag, manifest, notes_file, *, timeout=30, runner=subprocess.run):
    new_version = release_version(tag)
    for path in (Path(manifest), Path(notes_file)):
        if not path.is_file() or path.stat().st_size == 0:
            raise PublishError('Release manifest and notes must be existing, nonempty files.')

    github = GitHub(repository, timeout, runner)
    latest = github.read_release('latest')
    # Validate even an older latest before publishing anything. An unknown
    # version scheme needs an explicit migration, not an ordering guess.
    old_version = release_version(latest['tag_name']) if latest is not None else None
    if github.read_release(f'tags/{tag}') is not None:
        raise PublishError('Release already exists; refusing to overwrite it.')

    github.mutate(['create', tag, '--verify-tag', '--draft', '--latest=false',
                   '--title', tag, '--notes-file', str(notes_file), str(manifest)],
                  'Creating release draft')
    # Explicit false prevents GitHub's automatic latest selection when the
    # verified draft is first made public.
    github.mutate(['edit', tag, '--draft=false', '--latest=false'], 'Publishing release')
    promote = old_version is None or new_version > old_version
    if promote:
        github.mutate(['edit', tag, '--latest'], 'Promoting latest release')
    return promote


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--repository', required=True)
    parser.add_argument('--tag', required=True)
    parser.add_argument('--manifest', required=True, type=Path)
    parser.add_argument('--notes-file', required=True, type=Path)
    parser.add_argument('--timeout', type=int, default=30)
    args = parser.parse_args()
    try:
        promoted = publish(args.repository, args.tag, args.manifest, args.notes_file, timeout=args.timeout)
    except PublishError as exc:
        print(f'Release publication stopped: {exc}', file=sys.stderr)
        return 1
    print('Release published; latest advanced.' if promoted else 'Release published; latest retained.')
    return 0


if __name__ == '__main__':
    sys.exit(main())

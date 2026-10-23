import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from source_pair import resolve_pair, verify_php_runtime


class SourcePairGate(unittest.TestCase):
    def test_exact_tag_pair_and_rejection_paths(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "source-pair.json"
            lock = {"schemaVersion": 1, "release": "2026.10.23-r1", "phorgeCommit": "a" * 40}
            path.write_text(json.dumps(lock))
            self.assertEqual(resolve_pair(path, lock["release"]), "a" * 40)
            self.assertEqual(resolve_pair(path, lock["release"], "a" * 40), "a" * 40)
            for tag, sha in [("2026.10.22-r2", ""), ("2026.10.23-r1", "b" * 40),
                             ("2026.02.30-r1", ""), ("main", "")]:
                with self.subTest(tag=tag, sha=sha), self.assertRaises(ValueError):
                    resolve_pair(path, tag, sha)
            for key, value in [("schemaVersion", 2), ("phorgeCommit", "main"),
                               ("phorgeCommit", "a" * 39)]:
                changed = dict(lock); changed[key] = value
                path.write_text(json.dumps(changed))
                with self.subTest(key=key, value=value), self.assertRaises(ValueError):
                    resolve_pair(path, lock["release"])

    def test_old_or_unpinned_php_is_rejected_before_docker(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp)
            for image in ["php:8.3.35-apache", "php:8.3-apache", "php:8.2.36-apache"]:
                (path / "Dockerfile").write_text("ARG PHP_BASE_IMAGE=" + image + "\n")
                with self.subTest(image=image), self.assertRaises(ValueError):
                    verify_php_runtime(path, runner=lambda *a, **kw: self.fail("must not execute old PHP"))

    def test_effective_php_failure_stops_release(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp)
            (path / "docker/php").mkdir(parents=True)
            (path / "docker/php/uploads.ini").write_text("session.use_strict_mode=0\n")
            (path / "Dockerfile").write_text("ARG PHP_BASE_IMAGE=php:8.3.36-apache\n")
            (path / "deploy/acceptance").mkdir(parents=True)
            locked = "docker.io/library/php@sha256:" + "c" * 64
            (path / "deploy/acceptance/build-lock.json").write_text(json.dumps({"images": {"php": locked}}))
            seen = []
            def failed_runtime(command, **kwargs):
                seen.append(command[-4])
                self.assertEqual(command[:3], ["docker", "run", "--rm"])
                self.assertIn("session.upload_progress.enabled", command[-1])
                self.assertIn("session.use_strict_mode", command[-1])
                self.assertTrue(kwargs["check"])
                if command[-4] == locked:
                    raise subprocess.CalledProcessError(1, command)
            with self.assertRaises(subprocess.CalledProcessError):
                verify_php_runtime(path, runner=failed_runtime)
            self.assertEqual(seen, ["php:8.3.36-apache", locked])

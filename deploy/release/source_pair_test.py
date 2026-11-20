import json
from pathlib import Path
import subprocess
import shutil
import tempfile
import unittest
from source_pair import PHP_CHECK, resolve_pair, verify_php_runtime


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
            for image in ["php:8.3.36-apache", "php:8.4.27-apache", "php:8.5.11-apache", "php:8.5.12-apache",
                          "php:8.5-apache", "php:8.6.0-apache", "php:8.5.13RC1-apache", "php:8.5.13-dev-apache", "php:8.5.14RC1-apache"]:
                (path / "Dockerfile").write_text("ARG PHP_BASE_IMAGE=" + image + "\n")
                with self.subTest(image=image), self.assertRaises(ValueError):
                    verify_php_runtime(path, runner=lambda *a, **kw: self.fail("must not execute old PHP"))

    def run_php_predicate(self, version="8.5.13", major=8, minor=5, overrides=None):
        # Predicate controls only: substituted versions do not qualify the host
        # PHP executable or any published runtime. Production uses PHP_VERSION.
        php = shutil.which("php")
        self.assertIsNotNone(php, "PHP CLI is required for release guard tests")
        settings = {"session.upload_progress.enabled": "0",
                    "session.use_strict_mode": "1", "opcache.jit": "disable",
                    "opcache.jit_buffer_size": "0"}
        settings.update(overrides or {})
        args = [php]
        for key, value in settings.items():
            args.extend(["-d", key + "=" + value])
        program = PHP_CHECK.replace("PHP_MAJOR_VERSION", str(major)).replace(
            "PHP_MINOR_VERSION", str(minor)).replace("PHP_VERSION", json.dumps(version))
        probe = "$observed=[];foreach(" + json.dumps(list(settings)) + " as $key){$observed[$key]=ini_get($key);}" \
            + 'echo json_encode($observed)."\\n";'
        result = subprocess.run(args + ["-r", probe + program], capture_output=True)
        observed = json.loads(result.stdout.splitlines()[0])
        expected = dict(settings)
        # PHP's INI parser canonicalizes the literal "off" to an empty string.
        # It must differ from the safe "disable" control and still be refused.
        if expected["opcache.jit"] == "off":
            expected["opcache.jit"] = ""
        self.assertEqual(observed, expected, "PHP must apply each requested INI value")
        return result

    def test_actual_php_version_predicate(self):
        for version, major, minor, accepted in [
            ("8.5.12", 8, 5, False), ("8.5.13", 8, 5, True),
            ("8.5.14", 8, 5, True), ("8.5.13RC1", 8, 5, False),
            ("8.5.13-dev", 8, 5, False), ("8.5.14RC1", 8, 5, False),
            ("8.5.14-dev", 8, 5, False), ("8.4.99", 8, 4, False),
            ("8.6.0", 8, 6, False), ("9.0.0", 9, 0, False),
        ]:
            result = self.run_php_predicate(version, major, minor)
            with self.subTest(version=version):
                self.assertEqual(result.returncode, 0 if accepted else 1, result.stderr)
                if not accepted:
                    self.assertIn(b"below the security baseline", result.stderr)

    def test_actual_php_session_and_jit_predicates(self):
        self.assertEqual(self.run_php_predicate().returncode, 0)
        for overrides, message in [
            ({"session.upload_progress.enabled": "1"}, b"session security baseline"),
            ({"session.use_strict_mode": "0"}, b"session security baseline"),
            ({"opcache.jit": "off"}, b"JIT must be explicitly disabled"),
            ({"opcache.jit": "tracing"}, b"JIT must be explicitly disabled"),
            ({"opcache.jit_buffer_size": "1048576"}, b"JIT must be explicitly disabled"),
        ]:
            with self.subTest(overrides=overrides):
                result = self.run_php_predicate(overrides=overrides)
                self.assertEqual(result.returncode, 1, result.stderr)
                self.assertIn(message, result.stderr)
                self.assertNotIn(b"below the security baseline", result.stderr)

    def test_effective_php_failure_stops_release(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp)
            (path / "docker/php").mkdir(parents=True)
            (path / "docker/php/uploads.ini").write_text("session.use_strict_mode=0\n")
            (path / "docker/php/opcache.ini").write_text("opcache.jit=disable\nopcache.jit_buffer_size=0\n")
            (path / "Dockerfile").write_text("ARG PHP_BASE_IMAGE=php:8.5.13-apache\n")
            (path / "deploy/acceptance").mkdir(parents=True)
            locked = "docker.io/library/php@sha256:" + "c" * 64
            (path / "deploy/acceptance/build-lock.json").write_text(json.dumps({"images": {"php": locked}}))
            seen = []
            def failed_runtime(command, **kwargs):
                seen.append(command[-4])
                self.assertEqual(command[:3], ["docker", "run", "--rm"])
                self.assertIn("session.upload_progress.enabled", command[-1])
                self.assertIn("session.use_strict_mode", command[-1])
                self.assertIn("opcache.jit_buffer_size", command[-1])
                self.assertIn("phorge-opcache.ini:ro", command[-5])
                self.assertTrue(kwargs["check"])
                if command[-4] == locked:
                    raise subprocess.CalledProcessError(1, command)
            with self.assertRaises(subprocess.CalledProcessError):
                verify_php_runtime(path, runner=failed_runtime)
            self.assertEqual(seen, ["php:8.5.13-apache", locked])

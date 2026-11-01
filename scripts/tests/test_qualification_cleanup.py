"""Recovery from an interrupted qualification fault must not hide its cause."""

import contextlib
import io
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import qualify_release as qualification


class QualificationCleanupTest(unittest.TestCase):
    def test_killed_fault_client_can_be_purged(self):
        with tempfile.TemporaryDirectory() as temporary:
            data = Path(temporary) / 'data'
            blobs = data / 'sessions/session/blobs'
            blobs.mkdir(parents=True)
            (blobs / 'retained').write_text('evidence')
            normal = data / 'sessions/normal/blobs'
            normal.mkdir(parents=True, mode=0o750)
            normal.chmod(0o750)
            outside = Path(temporary) / 'outside'
            outside.mkdir(mode=0o700)
            link = data / 'sessions/link/blobs'
            link.parent.mkdir()
            link.symlink_to(outside, target_is_directory=True)
            purged = []

            def purge(*args):
                self.assertEqual(args, ('interceptor', 'purge', '--all'))
                self.assertEqual(blobs.stat().st_mode & 0o777, 0o700)
                self.assertEqual(normal.stat().st_mode & 0o777, 0o750)
                self.assertEqual(outside.stat().st_mode & 0o777, 0o700)
                shutil.rmtree(data)
                purged.append(True)

            with self.assertRaises(subprocess.TimeoutExpired):
                try:
                    # This is the actual failure mode: termination bypasses the
                    # fault client's finally/defer, leaving the directory at 0000.
                    subprocess.run([sys.executable, '-c',
                        'import os,sys,time; os.chmod(sys.argv[1],0); time.sleep(60)',
                        str(blobs)], timeout=1, check=True)
                finally:
                    with patch.object(qualification.subprocess, 'run') as docker:
                        qualification.cleanup_qualification(None, purge, 'interceptor', 'docker',
                            'inspection', ('wrapper', 'target'), data, True, sys.exc_info()[1])
                        self.assertEqual(docker.call_count, 3)
            self.assertEqual(purged, [True])
            self.assertFalse(data.exists())

    def test_cleanup_failures_preserve_timeout_and_attempt_remaining_resources(self):
        with tempfile.TemporaryDirectory() as temporary:
            error = subprocess.TimeoutExpired(['qualification-client'], 180)
            def purge(*args):
                raise PermissionError('purge failed')
            with self.assertRaises(subprocess.TimeoutExpired) as raised:
                try:
                    raise error
                finally:
                    with patch.object(qualification.subprocess, 'run', side_effect=RuntimeError('docker cleanup failed')) as docker, contextlib.redirect_stderr(io.StringIO()) as diagnostics:
                        qualification.cleanup_qualification(None, purge, 'interceptor', 'docker',
                            'inspection', ('wrapper', 'target'), Path(temporary), False, sys.exc_info()[1])
                        self.assertEqual(docker.call_count, 3)
                        self.assertIn('purge failed', diagnostics.getvalue())
            self.assertIs(raised.exception, error)

    def test_cleanup_failure_fails_an_otherwise_successful_qualification(self):
        with tempfile.TemporaryDirectory() as temporary:
            def purge(*args):
                raise PermissionError('purge failed')
            with patch.object(qualification.subprocess, 'run') as docker, contextlib.redirect_stderr(io.StringIO()):
                with self.assertRaisesRegex(RuntimeError, 'qualification cleanup failed'):
                    qualification.cleanup_qualification(None, purge, 'interceptor', 'docker',
                        'inspection', ('wrapper', 'target'), Path(temporary), False, None)
                self.assertEqual(docker.call_count, 3)


if __name__ == '__main__':
    unittest.main()

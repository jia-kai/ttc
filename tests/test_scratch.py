"""Self-contained checks for the Python visual/PTY drivers' scratch contract."""
import os
from pathlib import Path
import stat
import tempfile
import unittest

from scratch import private_scratch


class ScratchTests(unittest.TestCase):
    def test_create_and_recreate(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary) / 'scratch'
            private = private_scratch(root)
            self.assertEqual(stat.S_IMODE(root.stat().st_mode), 0o1777)
            self.assertEqual(stat.S_IMODE(private.stat().st_mode), 0o700)
            self.assertEqual(private.stat().st_uid, os.geteuid())
            private.rmdir()
            self.assertEqual(private_scratch(root), private)

    def test_permissions_ignore_restrictive_umask(self):
        with tempfile.TemporaryDirectory() as temporary:
            previous = os.umask(0o777)
            try:
                root = Path(temporary) / 'scratch'
                private = private_scratch(root)
                self.assertEqual(stat.S_IMODE(root.stat().st_mode), 0o1777)
                self.assertEqual(stat.S_IMODE(private.stat().st_mode), 0o700)
            finally:
                os.umask(previous)

    def test_reject_unsafe_paths_without_changing_them(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary) / 'scratch'
            root.mkdir(mode=0o700)
            with self.assertRaises(RuntimeError):
                private_scratch(root)
            self.assertEqual(stat.S_IMODE(root.stat().st_mode), 0o700)
            root.chmod(0o1777)
            private = private_scratch(root)
            private.chmod(0o755)
            with self.assertRaises(RuntimeError):
                private_scratch(root)
            private.rmdir()
            private.symlink_to(temporary, target_is_directory=True)
            with self.assertRaises(RuntimeError):
                private_scratch(root)

    def test_reject_shared_root_symlink(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary) / 'scratch'
            root.symlink_to(temporary, target_is_directory=True)
            with self.assertRaises(RuntimeError):
                private_scratch(root)


if __name__ == '__main__':
    unittest.main()

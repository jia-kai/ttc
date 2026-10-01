"""Shared sticky scratch root with private per-user test artifacts."""
import os
from pathlib import Path
import stat


def private_scratch(root=Path('/tmp/ttc')):
    """Create or validate a 1777 root and its owned 0700 effective-UID child."""
    root = Path(root)
    private = root / str(os.geteuid())
    for path, mode in ((root, 0o1777), (private, 0o700)):
        try:
            path.mkdir(mode=mode)
        except FileExistsError:
            pass
        else:
            path.chmod(mode)  # Make the contract independent of umask.
        metadata = path.lstat()
        if (not stat.S_ISDIR(metadata.st_mode)
                or stat.S_IMODE(metadata.st_mode) != mode
                or (path == private and metadata.st_uid != os.geteuid())):
            raise RuntimeError(f'unsafe scratch directory {path}: require {mode:04o}')
    return private

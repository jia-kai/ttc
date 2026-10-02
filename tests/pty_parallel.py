#!/usr/bin/env python3
"""Offline PTY check: concurrent instances, shared cwd/data, manual crash loading."""
import fcntl
import json
import os
from pathlib import Path
import pty
import select
import signal
import sqlite3
import struct
import subprocess
import tempfile
import termios
import time
import traceback

from scratch import private_scratch


class Instance:
    def __init__(self, root, name, responses, load=None):
        self.path = root / f'{name}-terminal.log'
        self.output = bytearray()
        self.cursor = 0
        script = root / f'{name}.json'
        script.write_text(json.dumps(responses))
        self.master, slave = pty.openpty()
        fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack('HHHH', 30, 100, 0, 0))
        command = [str(Path('./ttc').resolve()), '--plain', '--auto-name=false',
                   '--workdir', str(root / 'project'), '--data-dir', str(root / 'data'),
                   '--offline-script', str(script)]
        if load:
            command += ['--session', load]
        try:
            self.process = subprocess.Popen(command, stdin=slave, stdout=slave, stderr=slave,
                                            start_new_session=True)
        finally:
            os.close(slave)

    def send(self, text):
        os.write(self.master, text.encode() + b'\n')

    def read(self, seconds=0.05):
        if select.select([self.master], [], [], seconds)[0]:
            try:
                self.output.extend(os.read(self.master, 65536))
            except OSError:
                pass
        self.path.write_bytes(self.output)

    def expect(self, text):
        end = time.monotonic() + 10
        while time.monotonic() < end:
            at = self.output.find(text.encode(), self.cursor)
            if at >= 0:
                self.cursor = at + len(text.encode())
                return
            self.read()
        raise AssertionError(f'Missing {text!r}: {self.path}')

    def stop(self, kill=False):
        if self.process.poll() is None:
            if kill:
                os.killpg(self.process.pid, signal.SIGKILL)
            else:
                self.send('/quit')
            end = time.monotonic() + 5
            while self.process.poll() is None and time.monotonic() < end:
                self.read()
            if self.process.poll() is None:
                os.killpg(self.process.pid, signal.SIGKILL)
                raise AssertionError(f'Instance did not exit: {self.path}')
            self.process.wait()
            if not kill:
                assert self.process.returncode == 0, self.path
        self.read(0)
        os.close(self.master)


def main():
    root = Path(tempfile.mkdtemp(prefix='pty-parallel-', dir=private_scratch()))
    (root / 'project').mkdir()
    print(f'Artifacts: {root}', flush=True)
    instances = []
    first = None
    try:
        first = Instance(root, 'first', [{'calls': [{
            'id': 'shell-one', 'name': 'shell',
            'arguments': {'command': 'touch started; while [ ! -e release ]; do sleep 0.02; done'},
        }]}, {'text': 'First finished'}])
        instances.append(first)
        first.expect('/help')
        first.send('First task')
        end = time.monotonic() + 10
        while not (root / 'project' / 'started').exists():
            first.read()
            assert time.monotonic() < end, first.path
        with sqlite3.connect(root / 'data' / 'history.sqlite') as db:
            source = db.execute('SELECT session_id FROM turns WHERE status=?', ('running',)).fetchone()[0]

        second = Instance(root, 'second', [{'calls': [{
            'id': 'write-two', 'name': 'write',
            'arguments': {'path': 'second.txt', 'content': 'parallel edit'},
        }]}, {'text': 'Second finished'}, {'text': 'Loaded independently'}])
        instances.append(second)
        second.expect('/help')
        second.send('Second task')
        second.expect('Second finished')
        second.expect('Turn complete')
        assert (root / 'project' / 'second.txt').read_text() == 'parallel edit'
        with sqlite3.connect(root / 'data' / 'history.sqlite') as db:
            assert db.execute('SELECT status FROM turns WHERE session_id=?', (source,)).fetchone()[0] == 'running'
            assert db.execute('SELECT result_json FROM tool_calls WHERE session_id=?', (source,)).fetchone()[0] is None

        second.send('/load ' + source)
        second.expect('Loaded ·')
        second.send('Continue in another instance')
        second.expect('Loaded independently')
        second.expect('Turn complete')
        first.stop(kill=True)
        instances.remove(first)
        # Release the fixture shell left behind by SIGKILL. Restart deliberately
        # leaves the unfinished source records unchanged.
        (root / 'project' / 'release').touch()

        third = Instance(root, 'third', [{'text': 'Restart continued'}], load=source)
        instances.append(third)
        third.expect('/help')
        third.send('Continue after crash')
        third.expect('Restart continued')
        third.expect('Turn complete')
        with sqlite3.connect(root / 'data' / 'history.sqlite') as db:
            assert db.execute('SELECT status FROM turns WHERE session_id=?', (source,)).fetchone()[0] == 'running'
            assert db.execute('SELECT result_json FROM tool_calls WHERE session_id=?', (source,)).fetchone()[0] is None
            assert db.execute('SELECT count(*) FROM sessions').fetchone()[0] == 4
            assert db.execute('PRAGMA foreign_key_check').fetchall() == []
        for instance in list(instances):
            instance.stop()
            instances.remove(instance)
        print('PASS: parallel shared-store/workspace instances, isolated manual loads, no recovery')
    except BaseException:
        (root / 'failure.txt').write_text(traceback.format_exc())
        raise
    finally:
        (root / 'project' / 'release').touch()
        for instance in instances:
            instance.stop(kill=True)


if __name__ == '__main__':
    main()

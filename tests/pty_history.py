#!/usr/bin/env python3
"""Offline PTY regression for saved prompt recall and Ctrl-R search after restart."""
import fcntl
import json
import os
from pathlib import Path
import pty
import select
import shlex
import signal
import sqlite3
import struct
import subprocess
import termios
import tempfile
import time
import traceback

from scratch import private_scratch


def main():
    root = Path(tempfile.mkdtemp(prefix='pty-history-', dir=private_scratch()))
    print(f'Artifacts: {root}', flush=True)
    project = root / 'project'
    project.mkdir()
    data = root / 'data'
    older, newer = 'Older saved alpha prompt', 'Newer saved beta prompt'
    script = root / 'script.json'
    script.write_text(json.dumps([
        {'prefix': 'user: ' + older, 'text': 'Older seed saved.'},
        {'prefix': 'user: ' + newer, 'text': 'Newer seed saved.'},
    ]))
    editor = root / 'observe_editor.py'
    observed = root / 'drafts.jsonl'
    editor.write_text("from pathlib import Path\nimport json, sys\n"
                      "root = Path(__file__).parent\n"
                      "with (root / 'drafts.jsonl').open('a') as out:\n"
                      "    out.write(json.dumps(Path(sys.argv[-1]).read_text()) + '\\n')\n")
    binary = str(Path('./ttc').resolve())
    command = [binary, '--offline-script', str(script), '--workdir', str(project),
               '--data-dir', str(data), '--auto-name=false']
    process = master = None
    output, cursor, phase = bytearray(), 0, 'seed'

    def save():
        (root / f'{phase}-terminal.log').write_bytes(output)

    def read_for(seconds):
        end = time.monotonic() + seconds
        while time.monotonic() < end:
            if select.select([master], [], [], min(0.05, max(0, end-time.monotonic())))[0]:
                try:
                    chunk = os.read(master, 65536)
                except OSError:
                    break
                if not chunk:
                    break
                output.extend(chunk)
        save()

    def expect(text, timeout=8):
        nonlocal cursor
        end = time.monotonic() + timeout
        while time.monotonic() < end:
            at = output.find(text.encode(), cursor)
            if at >= 0:
                cursor = at + len(text.encode())
                return
            read_for(0.05)
        save()
        raise AssertionError(f'Missing {phase} output: {text}')

    def start(plain=False):
        nonlocal process, master, output, cursor
        output, cursor = bytearray(), 0
        master, slave = pty.openpty()
        fcntl.ioctl(master, termios.TIOCSWINSZ, struct.pack('HHHH', 36, 120, 0, 0))
        env = dict(os.environ, TERM='xterm-256color', COLORTERM='truecolor',
                   VISUAL='python3 ' + shlex.quote(str(editor)))
        env.pop('TMUX', None)
        try:
            process = subprocess.Popen(command + (['--plain'] if plain else []),
                                       stdin=slave, stdout=slave, stderr=slave,
                                       env=env, start_new_session=True,
                                       preexec_fn=lambda: fcntl.ioctl(0, termios.TIOCSCTTY, 0))
        finally:
            os.close(slave)

    def send(keys):
        os.write(master, keys)

    def stop(keys):
        nonlocal master, process
        send(keys)
        end = time.monotonic()+8
        while process.poll() is None and time.monotonic() < end:
            read_for(0.05)
        assert process.poll() == 0, f'TTC failed to exit: {process.poll()}'
        read_for(0.1)
        assert b'Error:' not in output
        os.close(master)
        master = process = None

    def inspect_draft(want, count):
        send(b'\x18e')
        end = time.monotonic()+5
        values = []
        while time.monotonic() < end:
            read_for(0.05)
            if observed.exists():
                values = [json.loads(line) for line in observed.read_text().splitlines()]
                if len(values) == count:
                    break
        assert len(values) == count and values[-1] == want, (count, values)
        read_for(0.15)  # Resume the terminal before sending more keys.

    try:
        start(plain=True)
        expect('/help')
        for text, answer in [(older, 'Older seed saved.'), (newer, 'Newer seed saved.')]:
            send(text.encode()+b'\n')
            expect(answer)
            expect('Turn complete')
        stop(b'/quit\n')
        phase = 'recall'
        start()
        expect('/help')
        send(b'unfinished draft\x1b[A')
        inspect_draft(newer, 1)
        send(b'\x1b[B')
        inspect_draft('unfinished draft', 2)
        send(b'\x12')
        expect('Prompt history search')
        send(b'alpha\r')
        inspect_draft(older, 3)
        send(b'\x12unlikely-no-match')
        expect('No matching prompts')
        send(b'\r\x1b')
        read_for(0.15)
        inspect_draft(older, 4)
        send(b'\x12\x1b[200~beta\x1b[201~\r')
        inspect_draft(newer, 5)
        send(b'\x18n')  # Session switch preserves prompt recall.
        expect('session_')
        send(b'\x1b[A\x1b[A')  # Skip the current-run /new command.
        inspect_draft(newer, 6)
        with sqlite3.connect(data / 'history.sqlite') as db:
            assert db.execute('SELECT count(*) FROM sessions').fetchone()[0] == 1
            assert db.execute('SELECT count(*) FROM model_requests').fetchone()[0] == 2
        stop(b'\x03')
        print(f'PASS: restart/session recall, draft restoration, Ctrl-R selection/cancel/paste, no inference; artifacts: {root}')
    except BaseException:
        save()
        (root / 'failure.txt').write_text(traceback.format_exc())
        raise
    finally:
        if process is not None and process.poll() is None:
            try:
                os.killpg(process.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            process.wait()
        if master is not None:
            os.close(master)


if __name__ == '__main__':
    main()

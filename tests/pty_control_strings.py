#!/usr/bin/env python3
"""Offline PTY regression for tcell's one-second control-string inactivity timeout.

Run: python3 tests/pty_control_strings.py --binary ./ttc
No graphics response mock or custom input parser is used. Full terminal, sent-byte,
history and failure artifacts are retained in the printed private scratch directory.
"""
import argparse
import errno
import fcntl
import json
import os
from pathlib import Path
import pty
import select
import signal
import sqlite3
import struct
import tempfile
import termios
import time
import traceback

from scratch import private_scratch


# DCS enters tcell's XDA parser; use an ASCII terminal identity, not binary data.
STRINGS = (
    ('APC', b'\x1b_', b'Gi=42;unfinished'),
    ('OSC', b'\x1b]', b'0;unfinished terminal title'),
    ('PM', b'\x1b^', b'unfinished private message'),
    ('SOS', b'\x1bX', b'unfinished string'),
    ('DCS', b'\x1bP', b'>|XTerm(390)'),
)
IDLE_SECONDS = 1.4  # Official timeout is 1s; leave scheduling margin.
FRAGMENT_SECONDS = 0.15  # Deliberately distinct writes, below that timeout.


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary', default='./ttc')
    args = parser.parse_args()
    binary = str(Path(args.binary).resolve())
    root = Path(tempfile.mkdtemp(prefix='pty-control-strings-', dir=private_scratch()))
    print(f'PTY artifacts: {root}', flush=True)
    project = root / 'project'
    project.mkdir(mode=0o700)
    for name in ('home', 'config', 'cache', 'xdg-data'):
        (root / name).mkdir(mode=0o700)
    data = root / 'data'
    expected_inputs = ['history seed'] + [
        f'retained {name} draft + recovered {name}' for name, _, _ in STRINGS
    ] + ['retained fragmented APC draft + typed after APC',
         'retained split ST draft + typed after ST']
    expected_replies = [f'Control fixture reply {i} OK.' for i in range(len(expected_inputs))]
    script = root / 'script.json'
    script.write_text(json.dumps([
        {'prefix': 'user: ' + text, 'text': reply}
        for text, reply in zip(expected_inputs, expected_replies)
    ], indent=2))
    argv = [binary, '--offline-script', str(script), '--data-dir', str(data),
            '--workdir', str(project)]
    environment = {
        'PATH': os.environ.get('PATH', '/usr/bin:/bin'),
        'LANG': 'C.UTF-8', 'TERM': 'xterm',
        'HOME': str(root / 'home'), 'XDG_CONFIG_HOME': str(root / 'config'),
        'XDG_CACHE_HOME': str(root / 'cache'), 'XDG_DATA_HOME': str(root / 'xdg-data'),
    }
    (root / 'launch.json').write_text(json.dumps({'argv': argv, 'env': environment}, indent=2))
    output = bytearray()
    cursor = 0
    phase = 'launch'
    pid = 0
    fd = None
    terminal = (root / 'terminal.log').open('wb')
    sent = (root / 'input.jsonl').open('w')
    started = time.monotonic()

    def read_for(seconds):
        end = time.monotonic() + seconds
        while time.monotonic() < end:
            wait = max(0, min(0.05, end - time.monotonic()))
            if select.select([fd], [], [], wait)[0]:
                try:
                    chunk = os.read(fd, 65536)
                except OSError as exc:
                    if exc.errno == errno.EIO:  # Linux PTY slave closed.
                        return
                    raise
                if not chunk:
                    return
                output.extend(chunk)
                terminal.write(chunk)
                terminal.flush()

    def expect(text, timeout=10):
        nonlocal cursor
        needle = text.encode()
        end = time.monotonic() + timeout
        while time.monotonic() < end:
            pos = output.find(needle, cursor)
            if pos >= 0:
                cursor = pos + len(needle)
                return
            read_for(0.05)
        raise AssertionError(f'{phase}: missing PTY output {text!r}')

    def send(payload):
        sent.write(json.dumps({'seconds': time.monotonic() - started,
                               'phase': phase, 'hex': payload.hex()}) + '\n')
        sent.flush()
        assert os.write(fd, payload) == len(payload), 'short PTY write'

    def history():
        with sqlite3.connect(data / 'history.sqlite', timeout=1) as db:
            messages = [(role, json.loads(content)['content']) for role, content in db.execute(
                "SELECT role,content_json FROM entries WHERE kind='message' "
                "AND role IN ('user','assistant') ORDER BY id")]
            statuses = [row[0] for row in db.execute(
                "SELECT status FROM turns WHERE trigger='user' ORDER BY id")]
        return {'messages': messages, 'statuses': statuses}

    def verify_history(count):
        observed = history()
        (root / 'history.json').write_text(json.dumps(observed, indent=2))
        users = [text for role, text in observed['messages'] if role == 'user']
        replies = [text for role, text in observed['messages'] if role == 'assistant']
        assert users == expected_inputs[:count], (phase, 'user inputs', users)
        assert replies == expected_replies[:count], (phase, 'model replies', replies)
        assert observed['statuses'] == ['completed'] * count, (phase, observed['statuses'])

    def submit(count, suffix):
        send(suffix + b'\r')
        expect(expected_replies[count - 1])
        expect('Turn complete')
        verify_history(count)

    try:
        pid, fd = pty.fork()
        if pid == 0:
            os.execve(binary, argv, environment)  # No inherited TMUX, graphics or credentials.
        fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack('HHHH', 38, 124, 0, 0))
        expect('/help')
        # Finish startup queries/timeouts before opening a deliberately incomplete string.
        read_for(IDLE_SECONDS)
        phase = 'Ctrl-C clears nonempty prompt'
        send('discarded α draft'.encode())
        read_for(FRAGMENT_SECONDS)
        send(b'\x03')
        read_for(FRAGMENT_SECONDS)
        phase = 'history seed'
        submit(1, b'history seed')
        for count, (name, introducer, payload) in enumerate(STRINGS, start=2):
            phase = f'{name} unfinished timeout'
            send(f'retained {name} draft'.encode())
            read_for(FRAGMENT_SECONDS)
            send(introducer + payload)
            read_for(FRAGMENT_SECONDS)
            # History recall and cursor movement must be swallowed, as must CR and
            # printable text. Exact subsequent DB input catches any draft mutation.
            send(b'\x1b[A\x1b[D\x1b[H swallowed payload\r')
            read_for(FRAGMENT_SECONDS)
            verify_history(count - 1)
            # Start the idle clock after the final payload/navigation bytes. No ST
            # (or BEL/cancel byte) is sent in any unfinished-string test.
            read_for(IDLE_SECONDS)
            submit(count, f' + recovered {name}'.encode())

        phase = 'valid fragmented APC'
        send(b'retained fragmented APC draft')
        read_for(FRAGMENT_SECONDS)
        # Keep ESC_ together: a bare ESC has its own shorter (50ms) ambiguity
        # timeout, distinct from the established control-string inactivity timer.
        # Total duration exceeds one second, but each gap is shorter: the
        # deadline must be inactivity-based, not an absolute string lifetime.
        for fragment in (b'\x1b_', b'Gi=42;', b'fragment', b'ed payload', b'\x1b\\'):
            send(fragment)
            read_for(0.4)
        submit(7, b' + typed after APC')

        phase = 'valid APC split ST'
        send(b'retained split ST draft')
        read_for(FRAGMENT_SECONDS)
        send(b'\x1b_Gi=43;OK')
        read_for(FRAGMENT_SECONDS)
        send(b'\x1b')  # ST's ESC arrives alone; backslash and typing arrive later.
        read_for(FRAGMENT_SECONDS)
        send(b'\\ + typed after ST\r')
        expect(expected_replies[7])
        expect('Turn complete')
        verify_history(8)

        phase = 'graceful Ctrl-C'
        send(b'\x03')
        deadline = time.monotonic() + 5
        while time.monotonic() < deadline:
            read_for(0.05)
            waited, status = os.waitpid(pid, os.WNOHANG)
            if waited:
                pid = 0
                assert os.waitstatus_to_exitcode(status) == 0, status
                break
        else:
            raise AssertionError('Ctrl-C did not exit TTC within five seconds')
        print(f'PASS: APC/OSC/PM/SOS/DCS idle recovery without ST, fragmented APC, '
              f'split ST, exact persisted inputs/replies and Ctrl-C clear/exit: {root}')
    except BaseException:
        # Persist the complete first failure before any reproduction. Never delete
        # isolated data or overwrite artifacts from an earlier run.
        (root / 'failure.log').write_text(f'Phase: {phase}\n' + traceback.format_exc())
        print(f'FAIL; complete artifacts: {root}', flush=True)
        raise
    finally:
        if pid:
            try:
                os.killpg(pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            os.waitpid(pid, 0)
        if fd is not None:
            read_for(0.1)
            os.close(fd)
        terminal.close()
        sent.close()


if __name__ == '__main__':
    main()

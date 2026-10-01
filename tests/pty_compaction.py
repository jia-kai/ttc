#!/usr/bin/env python3
"""Exercise automatic compaction and instruction/skill discovery in a real PTY."""
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


def main():
    root = Path(tempfile.mkdtemp(prefix='pty-compaction-', dir=private_scratch()))
    project = root / 'project'
    cwd = project / 'nested'
    cwd.mkdir(parents=True, mode=0o700)
    (project / 'AGENTS.md').write_text('Preserve reproducible experiment fixtures.')
    skill = project / '.agents/skills/fixture/SKILL.md'
    skill.parent.mkdir(parents=True, mode=0o700)
    skill.write_text('---\nname: fixture\ndescription: Inspect the reproducible fixture.\n---\nKeep experiment units explicit.\n')
    script = root / 'script.json'
    responses = [
        {'prefix': f'user: grow {i}', 'text': 'earlier evidence ' * 1600 + f'\nGrowth {i} complete.'}
        for i in range(1, 4)
    ]
    responses.extend([
        {'prefix': 'user: Focus:', 'text': '## Research checkpoint\n\nThree earlier research rounds completed; continue.'},
        {'prefix': 'user: grow 4', 'text': 'Automatic continuation verified.'},
    ])
    script.write_text(json.dumps(responses))
    binary = str(Path('./ttc').resolve())
    data = root / 'data'
    pid, fd = pty.fork()
    if pid == 0:
        os.environ.update(TERM='xterm-256color', COLORTERM='truecolor')
        os.environ.pop('TMUX', None)
        os.execv(binary, [binary, '--offline-script', str(script), '--data-dir', str(data),
                         '--workdir', str(cwd), '--auto-name=false'])
    fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack('HHHH', 38, 124, 0, 0))
    output, cursor = bytearray(), 0
    finished = False

    def save():
        (root / 'terminal.log').write_bytes(output)

    def expect(text, timeout=15):
        nonlocal cursor
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            found = output.find(text.encode(), cursor)
            if found >= 0:
                cursor = found + len(text.encode())
                save()
                return
            if select.select([fd], [], [], 0.05)[0]:
                try:
                    chunk = os.read(fd, 65536)
                except OSError:
                    break
                if not chunk:
                    break
                output.extend(chunk)
        save()
        raise AssertionError('Missing PTY output: ' + text)

    try:
        expect('/help')
        for i in range(1, 4):
            os.write(fd, f'grow {i}\r'.encode())
            expect(f'Growth {i} complete.')
            expect('Turn completed')
        os.write(fd, b'grow 4\r')
        expect('Automatic continuation verified.')
        expect('Turn completed')
        with sqlite3.connect(data / 'history.sqlite') as db:
            sessions = db.execute('SELECT id,read_only,predecessor_id FROM sessions').fetchall()
            assert len(sessions) == 2 and sum(row[1] for row in sessions) == 1, sessions
            current = next(row[0] for row in sessions if not row[1])
            assert db.execute("SELECT count(*) FROM model_requests WHERE purpose='compaction'").fetchone()[0] == 1
            assert db.execute("SELECT count(*) FROM turns WHERE status='completed'").fetchone()[0] == 4
            contexts = [json.loads(row[0]) for row in db.execute(
                "SELECT content_json FROM entries WHERE session_id=? AND role='developer'", (current,))]
            project_context = next(json.loads(message['content'])['project'] for message in contexts
                                   if 'project' in json.loads(message['content']))
            assert any(item['path'] == str(project / 'AGENTS.md') for item in project_context['instructions'])
            assert any(item['name'] == 'fixture' and item['path'] == str(skill)
                       for item in project_context['available_skills'])
            archive = Path(db.execute('SELECT archive_path FROM compactions').fetchone()[0])
        assert archive.exists() and Path(str(archive) + '.jsonl').exists()
        os.write(fd, b'\x03')
        # Drain pending frames while joining: terminal writes can otherwise
        # fill the PTY buffer and prevent the child from processing Ctrl-C.
        deadline = time.monotonic() + 10
        while True:
            exited, status = os.waitpid(pid, os.WNOHANG)
            if exited:
                finished = True
                break
            if time.monotonic() >= deadline:
                raise AssertionError('TTC did not exit after Ctrl-C')
            if select.select([fd], [], [], 0.05)[0]:
                try:
                    output.extend(os.read(fd, 65536))
                except OSError:
                    pass
        save()
        assert os.waitstatus_to_exitcode(status) == 0, status
        print('PASS: automatic continuation, archive, ancestor instructions and skills; artifacts:', root)
    except BaseException:
        save()
        (root / 'failure.txt').write_text(traceback.format_exc())
        print('FAIL; complete artifacts:', root)
        raise
    finally:
        if not finished:
            try:
                os.killpg(pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            os.waitpid(pid, 0)
        os.close(fd)


if __name__ == '__main__':
    main()

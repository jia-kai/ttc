#!/usr/bin/env python3
"""Offline real-terminal regression for the editor, completion, help and export."""
import argparse
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
import termios
import tempfile
import time
import traceback


from pty_ready import INPUT_READY
from scratch import private_scratch

def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary', default='./ttc')
    args = parser.parse_args()
    scratch = private_scratch()
    root = Path(tempfile.mkdtemp(prefix='pty-input-', dir=scratch))
    project = root / 'project'
    project.mkdir(mode=0o700)
    attachment = project / 'fixture file.txt'
    attachment.write_text('attachment snapshot evidence')
    capture_budget = 8192
    capture_padding = 12000
    capture_prefix, capture_tail = 'ready\ndone\n', 'capture-tail\n'
    script = root / 'script.json'
    script.write_text(json.dumps([
        {'prefix': 'user: Edited in external editor.\nsecond line', 'text': 'Editor round completed.'},
        {'prefix': 'user: look @', 'text': 'Attachment round completed.'},
        {'prefix': 'user: first linesecond line', 'text': 'Input scroll round completed.'},
        {'prefix': 'user: first line\nsecond line', 'text': 'Shift+Enter round completed.'},
        {'prefix': 'user: pasted first line\npasted second line', 'text': 'Multiline paste round completed.'},
        {'prefix': 'user: exercise tools', 'text': 'Waiting for the fixture gate.', 'calls': [
            {'id': 'write', 'name': 'write', 'arguments': {'path': 'result.py', 'content': 'print("evidence")\n'}},
            {'id': 'gate', 'name': 'shell', 'arguments': {'command': f"printf 'ready\\n'; while [ ! -e release-main ]; do sleep 0.02; done; printf 'done\\n'; i=0; while [ $i -lt {capture_padding} ]; do printf '\\n'; i=$((i+1)); done; printf 'capture-tail\\n'"}},
            {'id': 'strict', 'name': 'shell', 'arguments': {'command': 'false; printf should-not-run'}},
        ]},
        {'prefix': 'user: explain result.py', 'text': '', 'calls': [
            {'id': 'aside-read', 'name': 'read', 'arguments': {'path': 'result.py'}},
        ]},
        {'prefix': 'tool:', 'text': '## Side answer\n\nThe fixture prints **evidence**.\n\n| File | Behavior |\n| --- | --- |\n| result.py | Prints evidence |'},
        {'prefix': 'tool:', 'text': 'Main fixture completed.'},
        {'prefix': 'user: promote shell', 'calls': [
            {'id': 'promoted-shell', 'name': 'shell', 'arguments': {
                'command': 'touch promotion.ready; while [ ! -e promotion.release ]; do sleep 0.02; done; printf promoted-done'}},
        ]},
        {'prefix': 'user: steer promoted', 'text': 'Steering and promotion verified.'},
        {'prefix': 'user: queued after promotion', 'text': 'Queued turn verified.'},
    ]))
    editor = root / 'fake editor.py'
    editor.write_text('''import json, os, sys
from pathlib import Path
path = Path(sys.argv[-1])
assert path.stat().st_mode & 0o777 == 0o600
text = path.read_text()
(Path(__file__).parent / 'editor-observed.json').write_text(json.dumps({'draft': text, 'argv': sys.argv[1:]}))
if text == 'fail draft':
    sys.exit(2)
path.write_text('Edited in external editor.\\nsecond line  \\n\\n \\t')
''')
    binary = str(Path(args.binary).resolve())
    data = root / 'data'
    pid, fd = pty.fork()
    if pid == 0:
        os.environ.update(TERM='xterm-256color', COLORTERM='truecolor',
                          VISUAL='python3 ' + shlex.quote(str(editor)) + ' --visual',
                          EDITOR='this-editor-must-not-run')
        os.environ.pop('TMUX', None)
        os.execv(binary, [binary, '--offline-script', str(script), '--data-dir', str(data), '--workdir', str(project)])
    fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack('HHHH', 38, 124, 0, 0))
    output, cursor = bytearray(), 0
    finished = False

    def save():
        (root / 'terminal.log').write_bytes(output)

    def read_for(seconds):
        end = time.monotonic() + seconds
        while time.monotonic() < end:
            if select.select([fd], [], [], max(0, min(0.05, end - time.monotonic())))[0]:
                try:
                    chunk = os.read(fd, 65536)
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
            pos = output.find(text.encode(), cursor)
            if pos >= 0:
                cursor = pos + len(text.encode())
                save()
                return
            read_for(0.1)
        raise AssertionError('Missing PTY output: ' + text)

    def send(text):
        os.write(fd, text)

    try:
        expect(INPUT_READY)
        with sqlite3.connect(data / 'history.sqlite') as db:
            assert db.execute('SELECT count(*) FROM sessions').fetchone()[0] == 0
        send(b'/new\r')
        expect('session_')  # Wait for the command result, not the startup sidebar.
        read_for(0.2)
        send(b'\x18l')
        expect('Sessions')  # Cursor-addressed text may insert escapes at spaces.
        send(b'\x1b')
        read_for(0.2)
        with sqlite3.connect(data / 'history.sqlite') as db:
            assert db.execute('SELECT count(*) FROM sessions').fetchone()[0] == 0
        send('draft α'.encode() + b'\x18e')
        expect('Edited in external editor.')
        observed = json.loads((root / 'editor-observed.json').read_text())
        assert observed['draft'] == 'draft α' and observed['argv'][0] == '--visual', observed
        send(b'\r')
        expect('Editor round completed.')
        expect('Turn complete')
        send(b'fail draft\x18e')
        expect('Editor failed:')
        send(b'\x03')  # Failed editing preserves the input; clear the nonempty draft.
        send(b'look @fix')
        expect('fixture file.txt')
        send(b'\t')
        expect('Attached')
        attachment.write_text('changed after snapshot')
        send(b'\r')
        expect('Attachment round completed.')
        expect('Turn complete')
        # Ctrl+K at the start must not cut; Ctrl+J at the end must not insert LF.
        send(b'first line\x01\x0b\x05\x0asecond line')
        read_for(0.2)
        with sqlite3.connect(data / 'history.sqlite') as db:
            assert db.execute("SELECT count(*) FROM turns WHERE trigger='user'").fetchone()[0] == 2
        send(b'\r')
        expect('Input scroll round completed.')
        expect('Turn complete')
        # CSI-u modified Enter is supported by tcell; a literal LF is Ctrl+J.
        send(b'first line\x1b[13;2usecond line')
        read_for(0.2)
        with sqlite3.connect(data / 'history.sqlite') as db:
            assert db.execute("SELECT count(*) FROM turns WHERE trigger='user'").fetchone()[0] == 3
        send(b'\r')
        expect('Shift+Enter round completed.')
        expect('Turn complete')
        send(b'\x1b[200~pasted first line\npasted second line\x1b[201~')
        read_for(0.2)
        with sqlite3.connect(data / 'history.sqlite') as db:
            assert db.execute("SELECT count(*) FROM turns WHERE trigger='user'").fetchone()[0] == 4
        send(b'\r')
        expect('Multiline paste round completed.')
        expect('Turn complete')
        send(b'/rename Fixture inspection\r')
        deadline = time.monotonic() + 5
        while time.monotonic() < deadline:
            read_for(0.1)
            with sqlite3.connect(data / 'history.sqlite') as db:
                name, source = db.execute('SELECT name,name_source FROM sessions').fetchone()
            if (name, source) == ('Fixture inspection', 'manual'):
                break
        assert (name, source) == ('Fixture inspection', 'manual'), (name, source)
        read_for(0.2)
        send(b'/he\t\r')
        expect('TTC help')
        send(b'\x1b')
        read_for(0.2)
        send(b'exercise tools\r')
        expect('Waiting for the fixture gate.')
        deadline = time.monotonic() + 5
        while not (project / 'result.py').exists() and time.monotonic() < deadline:
            read_for(0.1)
        assert (project / 'result.py').read_text() == 'print("evidence")\n'
        send(b'/btw explain result.py\r')
        expect('read-only answer')
        expect('Side answer')
        with sqlite3.connect(data / 'history.sqlite') as db:
            assert db.execute("SELECT count(*) FROM turns WHERE status='running'").fetchone()[0] == 1
            row = db.execute("SELECT entry_id,record_json FROM tool_records r JOIN tool_calls c ON c.id=r.call_id WHERE c.name='write'").fetchone()
            write_entry = row[0]
            record = json.loads(row[1])
            assert '+print("evidence")' in record['markdown']['detail'], record
            assert 'Parameters:' in record['markdown']['detail'], record
        send(b'\x1b')
        read_for(0.2)
        send(('/inspect ' + str(write_entry) + '\r').encode())
        expect('Parameters:')
        send(b'\x1b')
        read_for(0.2)
        (project / 'release-main').write_text('release')
        expect('Main fixture completed.')
        expect('Turn complete')
        with sqlite3.connect(data / 'history.sqlite') as db:
            results = [json.loads(row[0]) for row in db.execute("SELECT result_json FROM tool_calls WHERE name='shell'")]
            gate = json.loads(db.execute("SELECT result_json FROM tool_calls WHERE provider_call_id='gate'").fetchone()[0])
            # Default head_tail selection preserves both sentinels without a
            # separator, bounded by source bytes rather than line count.
            expected_stdout = capture_prefix + '\n' * (capture_budget - len(capture_prefix) - len(capture_tail)) + capture_tail
            total_bytes = len(capture_prefix) + capture_padding + len(capture_tail)
            assert gate['stdout'] == expected_stdout and gate['truncated'], gate
            assert len(gate['stdout'].encode()) == capture_budget, gate
            assert gate['stdout_selection'] == {
                'head_bytes': capture_budget // 2, 'tail_bytes': capture_budget // 2,
                'total_bytes': total_bytes, 'omitted_bytes': total_bytes - capture_budget,
                'lost_prefix_bytes': 0,
            }, gate
            assert gate['stderr'] == '' and gate['stderr_selection'] == {
                'head_bytes': 0, 'tail_bytes': 0, 'total_bytes': 0,
                'omitted_bytes': 0, 'lost_prefix_bytes': 0,
            }, gate
            assert any(r.get('exit_code') == 1 and not r.get('stdout') for r in results), results
        export = root / 'conversation.md'
        send(('/export ' + str(export) + '\r').encode())
        expect('Exported')
        deadline = time.monotonic() + 5
        while not export.exists() and time.monotonic() < deadline:
            read_for(0.1)
        assert export.exists() and Path(str(export) + '.jsonl').exists()
        assert 'You are TTC' not in export.read_text()
        exact = Path(str(export) + '.jsonl').read_text()
        assert 'You are TTC' in exact
        with sqlite3.connect(data / 'history.sqlite') as db:
            messages = [json.loads(row[0])['content'] for row in db.execute(
                "SELECT content_json FROM entries WHERE kind='message' AND role='user' ORDER BY id")]
        assert messages[0] == 'Edited in external editor.\nsecond line', messages
        assert 'attachment snapshot evidence' in messages[1] and 'changed after snapshot' not in messages[1], messages
        assert messages[2] == 'first linesecond line', messages
        assert messages[3] == 'first line\nsecond line', messages
        assert messages[4] == 'pasted first line\npasted second line', messages
        # Inspect a human input without submitting the draft, then select its
        # pre-input checkpoint and restore the immutable tip without rerunning tools.
        with sqlite3.connect(data / 'history.sqlite') as db:
            session_id, final_tip = db.execute('SELECT id,active_entry_id FROM sessions').fetchone()
        send(b'retained branch draft\x18g')
        expect('User inputs')
        send(b'\x1b[H ')
        expect('user')
        send(b'\x1b')
        read_for(0.2)
        send(b'\x1b')
        read_for(0.2)  # Incremental terminal paints may keep the unchanged draft.
        send(b'\x03\x18g')  # Clear the nonempty draft before opening history.
        expect('User inputs')
        send(b'\x1b[H\r')  # Restore the checkpoint before the first human input.
        deadline = time.monotonic() + 5
        while (project / 'result.py').exists() and time.monotonic() < deadline:
            read_for(0.1)
        assert not (project / 'result.py').exists(), 'tree selection did not undo fixture edits'
        assert (project / 'release-main').exists(), 'shell side effect entered undo history'
        read_for(0.2)
        send(f'/branch {final_tip}\r'.encode())
        deadline = time.monotonic() + 5
        while not (project / 'result.py').exists() and time.monotonic() < deadline:
            read_for(0.1)
        assert (project / 'result.py').read_text() == 'print("evidence")\n'
        with sqlite3.connect(data / 'history.sqlite') as db:
            assert db.execute('SELECT active_entry_id FROM sessions WHERE id=?', (session_id,)).fetchone()[0] == final_tip
        send(b'promote shell\r')
        deadline = time.monotonic() + 5
        while not (project / 'promotion.ready').exists() and time.monotonic() < deadline:
            read_for(0.1)
        assert (project / 'promotion.ready').exists(), 'foreground promotion fixture did not start'
        send(b'steer promoted\r')  # Enter keeps this instruction in the current coding turn.
        expect('Steer')
        send(b'queued after promotion\x1b\r')  # Legacy Alt+Enter queues a separate turn.
        expect('Queued')
        send(b'\x02')  # Ctrl+B releases the foreground tool without canceling its process.
        expect('Steering and promotion verified.')
        expect('Queued turn verified.')
        expect('Turn complete')
        with sqlite3.connect(data / 'history.sqlite') as db:
            promoted = json.loads(db.execute("SELECT result_json FROM tool_calls WHERE provider_call_id='promoted-shell'").fetchone()[0])
            assert promoted['status'] == 'running', promoted
            turns = db.execute("SELECT count(DISTINCT turn_id) FROM model_requests WHERE purpose='coding' AND turn_id IN (SELECT turn_id FROM entries WHERE json_extract(content_json,'$.content')='promote shell')").fetchone()[0]
            assert turns == 1, turns
            inputs = db.execute("SELECT json_extract(content_json,'$.content'), json_extract(content_json,'$.input_source') FROM entries WHERE role='user' AND json_extract(content_json,'$.content') IN ('steer promoted','queued after promotion') ORDER BY id").fetchall()
            assert inputs == [('steer promoted', 'steer'), ('queued after promotion', 'queue')], inputs
        assert not (project / 'promotion.release').exists()
        (project / 'promotion.release').write_text('release')
        read_for(0.2)
        send(b'/clear\r')
        expect('session_')
        read_for(0.2)
        with sqlite3.connect(data / 'history.sqlite') as db:
            assert db.execute('SELECT count(*) FROM sessions').fetchone()[0] == 1
        send(b'\x03')
        read_for(0.2)
        _, status = os.waitpid(pid, 0)
        finished = True
        assert os.waitstatus_to_exitcode(status) == 0, status
        print('PASS: PTY editor/completion/export, input scrolling, Shift+Enter/multiline paste, history tree inspect/restore, steering/shell promotion, read-only btw popup, diff inspector and strict shell; artifacts:', root)
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

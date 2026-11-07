#!/usr/bin/env python3
"""Socket-free PTY regression for question input across inspection and copy mode."""
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
import subprocess
import termios
import tempfile
import time
import traceback

from pty_screen import Screen
from scratch import private_scratch


ROWS, COLUMNS = 52, 172
LABEL = 'question-audit'
CHILD_PROMPT = 'Keep the synthetic question audit running until released.'
QUESTION = 'Which custom evidence should be retained?'
COPY_HINT = 'Copy mode · updates paused · Esc returns'
FRAME_START, FRAME_END = b'\x1b[?2026h', b'\x1b[?2026l'


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary', default='./ttc')
    args = parser.parse_args()
    binary = str(Path(args.binary).resolve(strict=True))
    root = Path(tempfile.mkdtemp(prefix='pty-question-input-', dir=private_scratch()))
    print(f'Artifacts: {root}', flush=True)
    project, data = root / 'project', root / 'data'
    project.mkdir(mode=0o700)
    script = root / 'script.json'
    # The parent's sibling gate fixes scripted request ordering: the child must
    # start its blocking shell before the parent requests the question response.
    script.write_text(json.dumps([
        {'prefix': 'user: Exercise question input', 'calls': [
            {'id': 'audit', 'name': 'subagent', 'arguments': {
                'prompt': CHILD_PROMPT, 'label': LABEL, 'persistent': False,
                'background': True}},
            {'id': 'parent-gate', 'name': 'shell', 'arguments': {
                'command': 'while [ ! -e child.ready ]; do sleep 0.02; done; printf parent-gate-ready',
                'max_fg_wait_seconds': 30, 'timeout_seconds': 45}},
        ]},
        {'prefix': 'user: ' + CHILD_PROMPT, 'calls': [
            {'id': 'child-gate', 'name': 'shell', 'arguments': {
                'command': 'touch child.ready; while [ ! -e child.release ]; do sleep 0.02; done',
                'max_fg_wait_seconds': 120, 'timeout_seconds': 180}},
        ]},
        {'prefix': 'tool:', 'calls': [
            {'id': 'question', 'name': 'question', 'arguments': {'questions': [
                {'id': 'retain', 'prompt': 'Keep the inspection evidence?',
                 'recommended_option_id': 'keep',
                 'options': [{'id': 'keep', 'label': 'Keep evidence'},
                             {'id': 'discard', 'label': 'Discard evidence'}]},
                {'id': 'notes', 'prompt': QUESTION, 'options': []},
            ]}},
        ]},
        {'prefix': 'tool:', 'text': 'Question input preserved and submitted.'},
        {'prefix': 'tool:', 'text': 'Synthetic background audit complete.'},
        {'text': 'Background audit completion delivered.'},
    ], indent=2))
    command = [binary, '--offline-script', str(script), '--workdir', str(project),
               '--data-dir', str(data)]
    # Persist reproduction context before starting TTC, not after a failure.
    (root / 'context.json').write_text(json.dumps({
        'command': command, 'rows': ROWS, 'columns': COLUMNS,
        'binary_size': Path(binary).stat().st_size,
        'binary_mtime_ns': Path(binary).stat().st_mtime_ns,
        'term': 'xterm-256color', 'colorterm': 'truecolor',
    }, indent=2))
    master, slave = pty.openpty()
    fcntl.ioctl(master, termios.TIOCSWINSZ, struct.pack('HHHH', ROWS, COLUMNS, 0, 0))
    env = dict(os.environ, TERM='xterm-256color', COLORTERM='truecolor')
    env.pop('TMUX', None)
    process = None
    output, screen = bytearray(), Screen(ROWS, COLUMNS)
    database = data / 'history.sqlite'
    terminal_log = (root / 'terminal.log').open('wb', buffering=0)

    def history_state():
        if not database.exists():
            return {}
        try:
            with sqlite3.connect(f'file:{database}?mode=ro', uri=True, timeout=0.2) as db:
                return {
                    'turns': db.execute('SELECT id,actor_id,status FROM turns ORDER BY id').fetchall(),
                    'requests': db.execute('SELECT id,actor_id,purpose,status FROM model_requests ORDER BY id').fetchall(),
                    'tools': [(call, name, json.loads(result) if result else None)
                              for call, name, result in db.execute(
                                  'SELECT id,name,result_json FROM tool_calls ORDER BY id')],
                }
        except sqlite3.OperationalError as error:
            # Startup may create the file before committing its schema.
            return {'database_error': str(error)}

    def save(name=None):
        (root / 'screen.txt').write_text(screen.text)
        (root / 'state.json').write_text(json.dumps({
            'exit_status': process.poll() if process else None,
            'history': history_state(),
        }, indent=2))
        if name:
            (root / (name + '-screen.txt')).write_text(screen.text)

    def read_once(timeout=0.05):
        if not select.select([master], [], [], timeout)[0]:
            return
        try:
            chunk = os.read(master, 65536)
        except OSError as error:
            if error.errno != errno.EIO:
                raise
            return
        output.extend(chunk)
        terminal_log.write(chunk)
        screen.feed(chunk)

    def expect(*texts, absent=(), completed=None, timeout=12):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            state = history_state()
            if any(row[2] in ('failed', 'cancelled') for row in state.get('turns', [])) or any(
                    row[3] == 'failed' for row in state.get('requests', [])):
                raise AssertionError(f'TTC failed before rendering {texts!r}: {state}')
            # Cursor-addressed redraws can split anywhere, including a frame's
            # erase. Match reconstructed cells only after synchronized output.
            frame_complete = output.rfind(FRAME_START) <= output.rfind(FRAME_END)
            if (frame_complete and all(text in screen.text for text in texts)
                    and all(text not in screen.text for text in absent)
                    and (completed is None or sum(row[2] == 'completed' for row in
                                                 state.get('turns', [])) == completed)):
                save()
                return
            if process.poll() is not None:
                raise AssertionError(f'TTC exited {process.returncode} before rendering {texts!r}')
            read_once()
        raise AssertionError(f'Missing reconstructed PTY screen {texts!r}, absent={absent!r}; artifacts: {root}')

    def send(keys):
        os.write(master, keys)

    def click_line(needle, sidebar=False):
        # SGR reports use one-based cells. Locate the actual rendered row; do
        # not rely on a fixed sidebar y-coordinate or contiguous raw PTY bytes.
        for row, cells in enumerate(screen.cells):
            line = ''.join(cells)
            start, end = (COLUMNS - 32, COLUMNS) if sidebar else (0, COLUMNS - 32)
            column = line.find(needle, start, end)
            if column >= 0:
                x, y = column + 2, row + 1
                send(f'\x1b[<0;{x};{y}M\x1b[<0;{x};{y}m'.encode())
                return
        raise AssertionError(f'Cannot locate clickable row {needle!r}: {screen.text}')

    def expect_draft(text):
        expect('1 | [2] | Submit', QUESTION, 'Text: ' + text, 'Esc leaves text entry',
               absent=(COPY_HINT, 'Assignment prompt'))

    try:
        try:
            process = subprocess.Popen(command, stdin=slave, stdout=slave, stderr=slave,
                                       env=env, start_new_session=True,
                                       preexec_fn=lambda: fcntl.ioctl(0, termios.TIOCSCTTY, 0))
        finally:
            os.close(slave)
        expect('\n>\n')
        send(b'Exercise question input\r')
        expect('[1] | 2 | Submit', 'Keep evidence (Recommended)', LABEL)
        send(b'\r')
        expect('1 | [2] | Submit', QUESTION, 'Esc leaves text entry')
        send(b'keep-tail' + b'\x1b[D' * 4)
        expect_draft('keep-tail')
        save('before-inspection')

        # The question must not swallow the sidebar's live agent/job click.
        click_line(LABEL, sidebar=True)
        expect('Running job', 'Assignment prompt', CHILD_PROMPT)
        save('sidebar-job')
        send(b'\x1b')
        expect_draft('keep-tail')
        send(b'job-')
        expect_draft('keep-job-tail')  # Also verifies the insertion position.
        save('after-sidebar')

        # Transcript execution rows remain actionable above the inline form.
        click_line('subagent ·')
        expect('Parameters:', CHILD_PROMPT)
        save('transcript-tool')
        send(b'\x1b')
        expect_draft('keep-job-tail')
        send(b'row-')
        expect_draft('keep-job-row-tail')
        save('after-transcript')

        send(b'\x18f')
        expect(COPY_HINT, absent=(QUESTION, 'Text: keep-', '1 | [2] | Submit',
                                  'WORKSPACE', 'Context usage'))
        save('fullscreen')
        send(b'\x1b')
        expect_draft('keep-job-row-tail')
        send(b'copy-')
        expect_draft('keep-job-row-copy-tail')
        save('after-fullscreen')
        send(b'\r')
        expect('[Submit]', 'Submit answers', 'keep-job-row-copy-tail')
        send(b'\r')
        expect('Question input preserved and submitted.', 'Turn complete', completed=1)
        state = history_state()
        results = [result for _, name, result in state['tools'] if name == 'question']
        assert len(results) == 1, state
        (root / 'question-result.json').write_text(json.dumps(results[0], indent=2))
        assert results[0]['answers'] == [
            {'id': 'retain', 'values': ['keep'], 'source': 'option'},
            {'id': 'notes', 'values': ['keep-job-row-copy-tail'], 'source': 'custom'},
        ], results
        assert not results[0].get('dismissed'), results
        assert len(state['requests']) == 4, 'Inspection or fullscreen unexpectedly invoked inference'
        (project / 'child.release').touch()
        expect('Background audit completion delivered.', completed=3)
        save('submitted')
        send(b'/quit\r')
        deadline = time.monotonic() + 8
        while process.poll() is None and time.monotonic() < deadline:
            read_once()
        assert process.poll() == 0, f'TTC failed to exit cleanly: {process.poll()}'
        # Drain the final terminal reset/exit bytes into the durable log.
        while select.select([master], [], [], 0)[0]:
            before = len(output)
            read_once(0)
            if len(output) == before:
                break
        save('final')
        print(f'PASS: inline question sidebar/transcript clicks, Ctrl-X F/Escape, '
              f'preserved tab/text/cursor and exact submitted answers; artifacts: {root}')
    except BaseException:
        # Capture the original failure before diagnostic/cleanup work can fail.
        (root / 'failure.txt').write_text(traceback.format_exc())
        save()
        print(f'FAIL; complete artifacts: {root}', flush=True)
        raise
    finally:
        if process is not None and process.poll() is None:
            # Allow TTC to cancel its owned child/shell before falling back to
            # SIGKILL. Shells have independent process groups, unlike the TUI.
            os.killpg(process.pid, signal.SIGTERM)
            deadline = time.monotonic() + 3
            while process.poll() is None and time.monotonic() < deadline:
                read_once()
            if process.poll() is None:
                os.killpg(process.pid, signal.SIGKILL)
                process.wait()
        terminal_log.close()
        os.close(master)


if __name__ == '__main__':
    main()

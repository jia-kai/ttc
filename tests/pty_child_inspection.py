#!/usr/bin/env python3
"""Socket-free PTY regression for saved child-answer and assignment inspection."""
import argparse
from datetime import datetime, timezone
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
PREVIEW = 'CHILD-INSPECTION-PREVIEW'
TAIL = 'CHILD-INSPECTION-FULL-TAIL'
LABEL = 'pty-audit'
PROMPT = 'Inspect saved Markdown evidence; retain this assignment prompt.'


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary', default='./ttc')
    args = parser.parse_args()
    binary = str(Path(args.binary).resolve(strict=True))
    root = Path(tempfile.mkdtemp(prefix='pty-child-inspection-', dir=private_scratch()))
    print(f'Artifacts: {root}', flush=True)
    project, data = root / 'project', root / 'data'
    project.mkdir(mode=0o700)
    answer = (PREVIEW + '\n\n## Saved Markdown evidence\n\n'
              '**Strong evidence** with `inline code`.\n\n' +
              ''.join(f'Paragraph {i:03}: Reproducible evidence remains available beyond the bounded parent preview.\n\n'
                      for i in range(108)) +
              '\n## ' + TAIL + '\n\n- tail item **complete**\n')
    assert 8192 < len(answer.encode()) < 16000
    script = root / 'script.json'
    # The parent sibling gate prevents its next request from overtaking the
    # child's initial request. The child then waits until the test releases it.
    script.write_text(json.dumps([
        {'prefix': 'user: Start the background inspection', 'calls': [
            {'id': 'audit', 'name': 'subagent', 'arguments': {
                'prompt': PROMPT, 'label': LABEL, 'persistent': False,
                'background': True, 'easy': True}},
            {'id': 'parent-gate', 'name': 'shell', 'arguments': {
                'command': 'while [ ! -e child.ready ]; do sleep 0.02; done; printf parent-gate-ready',
                'max_fg_wait_seconds': 30, 'timeout_seconds': 45}},
        ]},
        {'prefix': 'user: ' + PROMPT, 'calls': [
            {'id': 'child-gate', 'name': 'shell', 'arguments': {
                'command': 'touch child.ready; while [ ! -e child.release ]; do sleep 0.02; done',
                'max_fg_wait_seconds': 30, 'timeout_seconds': 45}},
        ]},
        {'prefix': 'tool:', 'text': 'Background inspection is waiting.'},
        {'prefix': 'tool:', 'text': answer},
        {'text': 'Saved child completion delivered.'},
    ]))
    master, slave = pty.openpty()
    fcntl.ioctl(master, termios.TIOCSWINSZ, struct.pack('HHHH', ROWS, COLUMNS, 0, 0))
    env = dict(os.environ, TERM='xterm-256color', COLORTERM='truecolor')
    env.pop('TMUX', None)
    process = None
    output, screen = bytearray(), Screen(ROWS, COLUMNS)
    database = data / 'history.sqlite'

    def history_state():
        if not database.exists():
            return {'turns': [], 'requests': [], 'finishes': []}
        with sqlite3.connect(f'file:{database}?mode=ro', uri=True, timeout=0.2) as db:
            return {
                'turns': db.execute('SELECT id,actor_id,status,started_ms,finished_ms FROM turns ORDER BY started_ms,id').fetchall(),
                'requests': db.execute('SELECT id,actor_id,purpose,status FROM model_requests ORDER BY id').fetchall(),
                'finishes': [(entry, json.loads(content)) for entry, content in db.execute(
                    "SELECT id,content_json FROM entries WHERE json_extract(content_json,'$.type')='child_turn_finished' ORDER BY id")],
            }

    def save(name=None):
        (root / 'terminal.log').write_bytes(output)
        (root / 'screen.txt').write_text(screen.text)
        (root / 'state.json').write_text(json.dumps({
            'binary': binary, 'exit_status': process.poll() if process else None,
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
        screen.feed(chunk)
        (root / 'terminal.log').write_bytes(output)

    def expect(*texts, completed=None, terminal_completions=None, timeout=12):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            state = history_state()
            if any(row[2] in ('failed', 'cancelled') for row in state['turns']) or any(
                    row[3] == 'failed' for row in state['requests']):
                save()
                raise AssertionError(f'TTC failed before rendering {texts!r}: {state}')
            count = sum(row[2] == 'completed' for row in state['turns'])
            # tcell frames can span reads. Do not accept an intermediate paint
            # with old modal cells still present below freshly rendered text.
            frame_complete = output.rfind(b'\x1b[?2026h') <= output.rfind(b'\x1b[?2026l')
            if (frame_complete and all(text in screen.text for text in texts)
                    and (completed is None or count == completed)
                    and (terminal_completions is None or screen.text.count('Turn complete') == terminal_completions)):
                save()
                return
            if process.poll() is not None:
                save()
                raise AssertionError(f'TTC exited {process.returncode} before rendering {texts!r}')
            read_once()
        save()
        raise AssertionError(f'Missing reconstructed PTY screen {texts!r}; artifacts: {root}')

    def send(keys):
        os.write(master, keys)

    def key_frames(keys, count):
        # Every handled key redraws through tcell's synchronized-output fence.
        # Wait for those frames instead of sleeping or probing raw text patches.
        target = output.count(b'\x1b[?2026l') + count
        send(keys)
        deadline = time.monotonic() + 8
        while output.count(b'\x1b[?2026l') < target or output.rfind(b'\x1b[?2026h') > output.rfind(b'\x1b[?2026l'):
            if process.poll() is not None or time.monotonic() >= deadline:
                raise AssertionError('Terminal did not finish rendering keyboard navigation')
            read_once()

    def click(column, row):
        # SGR mouse coordinates are one-based; Screen coordinates are zero-based.
        send(f'\x1b[<0;{column + 1};{row + 1}M\x1b[<0;{column + 1};{row + 1}m'.encode())

    def click_line(needle, sidebar=False):
        for row, text in enumerate(screen.text.splitlines()):
            column = text.find(needle, COLUMNS - 32 if sidebar else 0)
            if column >= 0:
                click(column + min(2, len(needle) - 1), row)
                return
        raise AssertionError(f'Cannot locate clickable row {needle!r}: {screen.text}')

    try:
        try:
            process = subprocess.Popen([
                binary, '--offline-script', str(script), '--workdir', str(project),
                '--data-dir', str(data),
            ], stdin=slave, stdout=slave, stderr=slave, env=env, start_new_session=True,
                preexec_fn=lambda: fcntl.ioctl(0, termios.TIOCSCTTY, 0))
        finally:
            os.close(slave)
        expect('\n>\n')  # Screen strips the composer's trailing space.
        send(b'Start the background inspection\r')
        expect('Background inspection is waiting.', 'Turn complete', completed=1, terminal_completions=1)
        assert TAIL not in screen.text
        # Inspect the live sidebar job while the assignment is still running.
        click_line(LABEL, sidebar=True)
        expect('Assignment prompt', PROMPT)
        save('running-assignment')
        send(b'\x1b')
        expect('Background inspection is waiting.', '\n>\n')
        (project / 'child.release').write_text('release')
        expect('final answer ' + PREVIEW, 'Saved child completion delivered.', completed=3, terminal_completions=2)
        assert TAIL not in screen.text, 'Full child answer leaked into compact transcript preview'
        state = history_state()
        assert len(state['finishes']) == 1, state
        finish_entry, finish = state['finishes'][0]
        assert finish['easy'] is True and finish['persistent'] is False, finish
        assert finish['answer_truncated'] is True and answer.startswith(finish['answer']), finish
        assert len(finish['answer'].encode()) <= 8192 and TAIL not in finish['answer']
        with sqlite3.connect(database) as db:
            saved = json.loads(db.execute('SELECT content_json FROM entries WHERE id=?',
                                          (finish['result_entry_id'],)).fetchone()[0])
        assert saved['content'] == answer, 'Saved Markdown source was changed or clipped'
        child_turn = next(row for row in state['turns'] if row[0] == finish['child_turn_id'])
        def timestamp(milliseconds):
            seconds = datetime.fromtimestamp(milliseconds // 1000, timezone.utc).strftime('%Y-%m-%dT%H:%M:%S')
            fraction = (f'.{milliseconds % 1000:03d}'.rstrip('0') if milliseconds % 1000 else '')
            return seconds + fraction + 'Z'
        click_line('final answer ' + PREVIEW)
        expect('Final answer', 'D completion details', 'Agent details', finish['child_id'],
               finish['child_turn_id'], finish['job_id'], 'Easy: true', 'Started:', 'Finished:', 'Elapsed:',
               timestamp(child_turn[3]), timestamp(child_turn[4]))
        assert 'unavailable' not in screen.text, 'Fresh assignment metadata is incomplete'
        save('mouse-answer-metadata')
        send(b'\x1b[F')  # End: show content well beyond the bounded parent preview.
        expect(TAIL, 'tail item complete')
        assert '**complete**' not in screen.text, 'Answer opened as literal/JSON text, not Markdown'
        save('mouse-answer-tail')
        send(b'd')
        expect('child_turn_finished', finish['child_id'])
        save('completion-details')
        send(b'\x1b')
        expect('final answer ' + PREVIEW, '\n>\n')
        # Mouse activation does not set focus. Walk from the last publication
        # using Alt+Up/Tab, closing unrelated execution inspectors. This permits
        # either order of child-job and final-answer publication without using
        # screen-line counts as a proxy for logical transcript rows.
        for step in range(8):
            key_frames(b'\x1b[1;3A\t', 2)
            save(f'keyboard-step-{step}')
            if 'Final answer' in screen.text and 'Agent details' in screen.text:
                break
            if 'Esc closes' in screen.text:
                key_frames(b'\x1b', 1)
        else:
            raise AssertionError('Alt+Up/Tab could not activate the saved final-answer row')
        expect('Final answer', 'D completion details', 'Agent details', 'Easy: true')
        send(b'\x1b[F')
        expect(TAIL, 'tail item complete')
        save('keyboard-answer-tail')
        assert len(history_state()['requests']) == 5, 'Inspection unexpectedly invoked inference'
        # Script inference has no native provider-boundary capture rows. Request
        # row suffix/action behavior remains covered by the Go projection tests.
        send(b'\x1b')
        expect('\n>\n', 'Saved child completion delivered.')
        send(b'/quit\r')
        deadline = time.monotonic() + 8
        while process.poll() is None and time.monotonic() < deadline:
            read_once()
        assert process.poll() == 0, f'TTC failed to exit cleanly: {process.poll()}'
        save('final')
        print(f'PASS: live assignment prompt, saved Markdown beyond 8192-byte preview, '
              f'IDs/time/easy metadata, mouse/keyboard activation, D details; artifacts: {root}')
    except BaseException:
        save()
        (root / 'failure.txt').write_text(traceback.format_exc())
        print(f'FAIL; complete artifacts: {root}', flush=True)
        raise
    finally:
        if process is not None and process.poll() is None:
            os.killpg(process.pid, signal.SIGKILL)
            process.wait()
        os.close(master)


if __name__ == '__main__':
    main()

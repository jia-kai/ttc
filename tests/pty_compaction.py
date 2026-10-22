#!/usr/bin/env python3
"""Exercise automatic compaction and instruction/skill discovery in a real PTY."""
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
from pty_screen import Screen
from pty_compaction_reload import verify_compaction_context


def export_responses(root):
    """Exercise export and existing file tools after the compacted handoff."""
    markdown, exact = root / 'history.md', root / 'before-compaction.jsonl'
    return [
        {'prefix': 'user: export history', 'calls': [
            {'id': 'export-markdown', 'name': 'conversation_export',
             'arguments': {'path': str(markdown)}},
            {'id': 'export-exact', 'name': 'conversation_export',
             'arguments': {'path': str(exact), 'format': 'jsonl', 'scope': 'before_compaction'}},
        ]},
        {'calls': [
            {'id': 'lookup-original', 'name': 'grep',
             'arguments': {'path': str(markdown), 'pattern': 'Growth 1 complete.', 'literal': True}},
            {'id': 'read-export', 'name': 'read',
             'arguments': {'path': str(exact), 'offset': 1, 'limit': 1}},
        ]},
        {'text': 'On-demand history lookup verified.'},
        {'prefix': 'user: reject existing export', 'calls': [
            {'id': 'reject-existing', 'name': 'conversation_export',
             'arguments': {'path': str(markdown)}},
        ]},
        {'text': 'Existing export preserved.'},
    ]


def verify_exports(db, root, original):
    """Check actual tool outcomes and original precompaction evidence."""
    markdown = (root / 'history.md').read_bytes()
    assert markdown == original, 'Existing-target rejection changed exported contents'
    assert b'Growth 1 complete.' in markdown, 'Export lost original summarized evidence'
    exact = [json.loads(line) for line in (root / 'before-compaction.jsonl').read_text().splitlines()]
    encoded = json.dumps(exact)
    assert 'Growth 1 complete.' in encoded and 'Automatic continuation verified.' not in encoded
    rows = db.execute('SELECT provider_call_id,result_json FROM tool_calls').fetchall()
    outcomes = {call: json.loads(result) for call, result in rows}
    for call in ('export-markdown', 'export-exact', 'lookup-original', 'read-export'):
        assert outcomes[call]['ok'], (call, outcomes[call])
    assert not outcomes['reject-existing']['ok'], outcomes['reject-existing']
    assert 'Growth 1 complete.' in json.dumps(outcomes['lookup-original']), outcomes['lookup-original']


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary', type=Path, default=Path('./ttc'),
                        help='TTC executable (use a scratch build for investigation)')
    args = parser.parse_args()
    root = Path(tempfile.mkdtemp(prefix='pty-compaction-', dir=private_scratch()))
    project = root / 'project'
    cwd = project / 'nested'
    cwd.mkdir(parents=True, mode=0o700)
    (project / 'AGENTS.md').write_text('Preserve reproducible experiment fixtures.')
    skill = project / '.agents/skills/fixture/SKILL.md'
    skill.parent.mkdir(parents=True, mode=0o700)
    skill.write_text('---\nname: fixture\ndescription: Inspect the reproducible fixture.\n---\nKeep experiment units explicit.\n')
    script = root / 'script.json'
    # Three rounds cross coding's proactive trigger only before grow 4, while
    # leaving summary capacity for the original prefix plus system/tools and
    # the final developer directive in the scripted 32768-token context.
    responses = [
        {'prefix': f'user: grow {i}', 'text': 'earlier evidence ' * 1200 + f'\nGrowth {i} complete.'}
        for i in range(1, 4)
    ]
    responses.extend([
        # The summary directive is developer guidance; the offline prefix
        # matcher skips it. Verify its actual persisted request below instead.
        {'text': '## Research checkpoint\n\nThree earlier research rounds completed; continue.'},
        {'prefix': 'user: {"type":"retained_inputs",', 'text': 'Automatic continuation verified.'},
    ])
    responses.extend(export_responses(root))
    script.write_text(json.dumps(responses))
    binary = str(args.binary.resolve(strict=True))
    data = root / 'data'
    pid, fd = pty.fork()
    if pid == 0:
        # Establish geometry before TTC can render its first frame; resizing
        # only in the parent races startup on fast/loaded CI machines.
        fcntl.ioctl(0, termios.TIOCSWINSZ, struct.pack('HHHH', 38, 124, 0, 0))
        os.environ.update(TERM='xterm-256color', COLORTERM='truecolor')
        os.environ.pop('TMUX', None)
        os.execv(binary, [binary, '--offline-script', str(script), '--data-dir', str(data),
                         '--workdir', str(cwd), '--auto-name=false'])
    fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack('HHHH', 38, 124, 0, 0))
    output = bytearray()
    screen = Screen(38, 124)
    finished = False
    exit_status = None
    cleanup_exit_status = None

    def history_state():
        database = data / 'history.sqlite'
        if not database.exists():
            return {'turns': [], 'requests': []}
        try:
            with sqlite3.connect(f'file:{database}?mode=ro', uri=True, timeout=0.1) as db:
                return {
                    'turns': db.execute('SELECT id,status FROM turns ORDER BY started_ms').fetchall(),
                    'requests': db.execute('SELECT id,purpose,status,attempts_json FROM model_requests ORDER BY id').fetchall(),
                    'errors': db.execute("SELECT content_json FROM entries WHERE json_extract(content_json,'$.error') IS NOT NULL ORDER BY id").fetchall(),
                }
        except sqlite3.OperationalError as error:
            return {'turns': [], 'requests': [], 'read_error': str(error)}

    def save():
        (root / 'terminal.log').write_bytes(output)
        (root / 'screen.txt').write_text(screen.text)
        (root / 'state.json').write_text(json.dumps({
            'binary': binary, 'exit_status': exit_status,
            'cleanup_exit_status': cleanup_exit_status, 'history': history_state(),
        }, indent=2))

    def expect(*texts, completed_turns=None, timeout=15):
        nonlocal finished, exit_status
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            state = history_state()
            failed = [row for row in state['turns'] if row[1] in ('failed', 'cancelled')]
            if failed or any(row[2] == 'failed' for row in state['requests']):
                save()
                raise AssertionError(f'TTC failed before rendering {texts!r}; history: {state}')
            complete = sum(row[1] == 'completed' for row in state['turns'])
            if all(text in screen.text for text in texts) and (completed_turns is None or complete == completed_turns):
                save()
                return
            exited, status = os.waitpid(pid, os.WNOHANG)
            if exited:
                finished = True
                exit_status = os.waitstatus_to_exitcode(status)
                save()
                raise AssertionError(f'TTC exited with {exit_status} before rendering {texts!r}')
            if select.select([fd], [], [], 0.05)[0]:
                try:
                    chunk = os.read(fd, 65536)
                except OSError as error:
                    if error.errno != errno.EIO:
                        raise
                    # A closing PTY can report EIO just before waitpid observes
                    # exit. Recheck the process rather than losing its status.
                    time.sleep(0.01)
                    continue
                if not chunk:
                    time.sleep(0.01)
                    continue
                output.extend(chunk)
                screen.feed(chunk)
        save()
        raise AssertionError(f'Missing PTY screen output {texts!r}; completed turns wanted={completed_turns}; history: {history_state()}; artifacts: {root}')

    try:
        expect('/help')
        for i in range(1, 4):
            os.write(fd, f'grow {i}\r'.encode())
            # tcell can emit text as cursor-addressed patches, not contiguous
            # substrings, and completion/output can share a frame in any order.
            # Require both visible messages and this exact durable turn count.
            expect(f'Growth {i} complete.', 'Turn complete', completed_turns=i)
        os.write(fd, b'grow 4\r')
        expect('Automatic continuation verified.', 'Turn complete', completed_turns=4)
        with sqlite3.connect(data / 'history.sqlite') as db:
            sessions = db.execute('SELECT id,read_only,predecessor_id FROM sessions').fetchall()
            assert len(sessions) == 2 and sum(row[1] for row in sessions) == 1, sessions
            current = next(row[0] for row in sessions if not row[1])
            assert db.execute("SELECT count(*) FROM model_requests WHERE purpose='compaction'").fetchone()[0] == 1
            summary_messages = [json.loads(row[0]) for row in db.execute(
                "SELECT e.content_json FROM entries e JOIN model_requests r "
                "ON r.id=json_extract(e.content_json,'$.request_id') "
                "WHERE r.purpose='compaction' AND json_extract(e.content_json,'$.type')='request_message' "
                "ORDER BY e.id")]
            directive = summary_messages[-2]
            assert directive['role'] == 'developer' and directive['message']['type'] == 'runtime_instruction', directive
            assert directive['message']['content'].startswith('This conversation is about to be compacted.'), directive
            assert 'Do not execute tools' in directive['message']['content'], directive
            assert summary_messages[-1]['role'] == 'assistant', summary_messages[-1]
            assert summary_messages[-1]['message']['type'] == 'assistant', summary_messages[-1]
            verify_compaction_context(db, current, summary_messages[-1]['message']['content'])
            assert any(item['role'] == 'assistant' and 'Growth 1 complete.' in item['message'].get('content', '')
                       for item in summary_messages[:-2]), 'Summary request lost structured historical evidence'
            assert db.execute("SELECT count(*) FROM turns WHERE status='completed'").fetchone()[0] == 4
            retained = db.execute("SELECT content_json FROM entries WHERE session_id=? AND role='user' AND json_extract(content_json,'$.content')='grow 4'", (current,)).fetchall()
            assert len(retained) == 1, retained
            contexts = [json.loads(row[0]) for row in db.execute(
                "SELECT content_json FROM entries WHERE session_id=? AND role='developer' "
                "AND json_valid(json_extract(content_json,'$.content')) "
                "AND json_extract(content_json,'$.content') LIKE '{%'", (current,))]
            project_context = next(json.loads(message['content'])['project'] for message in contexts
                                   if json.loads(message['content']).get('type') == 'runtime_context'
                                   and 'project' in json.loads(message['content']))
            assert any(item['path'] == str(project / 'AGENTS.md') for item in project_context['instructions'])
            assert any(item['name'] == 'fixture' and item['path'] == str(skill)
                       for item in project_context['available_skills'])
            source_tip = db.execute('SELECT source_tip_id FROM compactions').fetchone()[0]
            assert source_tip > 0
        assert not list(data.glob('lineages/*/compactions')), 'Compaction eagerly created archive directories'
        os.write(fd, b'export history\r')
        expect('On-demand history lookup verified.', 'Turn complete', completed_turns=5)
        original = (root / 'history.md').read_bytes()
        os.write(fd, b'reject existing export\r')
        expect('Existing export preserved.', 'Turn complete', completed_turns=6)
        with sqlite3.connect(data / 'history.sqlite') as db:
            verify_exports(db, root, original)
        assert not list(data.glob('lineages/*/compactions')), 'On-demand export created eager archive directories'
        os.write(fd, b'\x03')
        # Drain pending frames while joining: terminal writes can otherwise
        # fill the PTY buffer and prevent the child from processing Ctrl-C.
        deadline = time.monotonic() + 10
        while True:
            exited, status = os.waitpid(pid, os.WNOHANG)
            if exited:
                finished = True
                exit_status = os.waitstatus_to_exitcode(status)
                break
            if time.monotonic() >= deadline:
                raise AssertionError('TTC did not exit after Ctrl-C')
            if select.select([fd], [], [], 0.05)[0]:
                try:
                    chunk = os.read(fd, 65536)
                    output.extend(chunk)
                    screen.feed(chunk)
                except OSError:
                    pass
        save()
        assert os.waitstatus_to_exitcode(status) == 0, status
        print('PASS: automatic continuation, on-demand history exports, existing-target safety, ancestor instructions and skills; artifacts:', root)
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
            _, status = os.waitpid(pid, 0)
            cleanup_exit_status = os.waitstatus_to_exitcode(status)
        # Preserve final output even for early exits or killed failed fixtures.
        while select.select([fd], [], [], 0)[0]:
            try:
                chunk = os.read(fd, 65536)
            except OSError:
                break
            if not chunk:
                break
            output.extend(chunk)
            screen.feed(chunk)
        save()
        os.close(fd)


if __name__ == '__main__':
    main()

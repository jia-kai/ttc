#!/usr/bin/env python3
"""Recover interrupted compaction notifications through the real offline CLI/PTY.

Only a stopped CLI's private fixture database is seeded; no live history is
modified. Every phase retains its terminal transcript, including on failure.
"""
import argparse
from dataclasses import dataclass
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
from typing import Callable

from scratch import private_scratch


class Terminal:
    def __init__(self, command, log):
        self.log = log
        self.output = bytearray()
        self.cursor = 0
        self.fd, slave = pty.openpty()
        fcntl.ioctl(self.fd, termios.TIOCSWINSZ, struct.pack('HHHH', 38, 124, 0, 0))
        env = dict(os.environ, TERM='xterm-256color', COLORTERM='truecolor')
        env.pop('TMUX', None)
        try:
            self.process = subprocess.Popen(
                command, stdin=slave, stdout=slave, stderr=slave, env=env,
                start_new_session=True,
                preexec_fn=lambda: fcntl.ioctl(0, termios.TIOCSCTTY, 0))
        except BaseException:
            os.close(self.fd)
            raise
        finally:
            os.close(slave)

    def save(self):
        self.log.write_bytes(self.output)

    def read_for(self, seconds):
        deadline = time.monotonic() + seconds
        while time.monotonic() < deadline:
            wait = min(0.05, max(0, deadline-time.monotonic()))
            if select.select([self.fd], [], [], wait)[0]:
                try:
                    chunk = os.read(self.fd, 65536)
                except OSError:
                    break
                if not chunk:
                    break
                self.output.extend(chunk)
        self.save()

    def expect(self, text, timeout=15):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            found = self.output.find(text.encode(), self.cursor)
            if found >= 0:
                self.cursor = found + len(text.encode())
                return
            self.read_for(0.05)
            if self.process.poll() is not None:
                break
        self.save()
        raise AssertionError(f'Missing PTY output {text!r}; transcript: {self.log}')

    def send(self, keys):
        os.write(self.fd, keys)

    def stop(self, keys=b'\x03'):
        self.send(keys)
        deadline = time.monotonic() + 10
        # Drain while joining so terminal writes cannot block graceful shutdown.
        while self.process.poll() is None and time.monotonic() < deadline:
            self.read_for(0.05)
        self.read_for(0.1)
        assert self.process.poll() == 0, f'CLI exit status: {self.process.poll()}'
        assert b'Error:' not in self.output, self.log

    def close(self):
        try:
            if self.process.poll() is None:
                try:
                    os.killpg(self.process.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
                self.process.wait()
        finally:
            self.save()
            os.close(self.fd)


def source_state(db, source):
    """Include all history except the startup retention-protection activity clock."""
    state = {
        table: db.execute(f'SELECT * FROM {table} WHERE {key}=? ORDER BY id',
                          (source,)).fetchall()
        for table, key in [('sessions', 'id'), ('entries', 'session_id'),
                           ('turns', 'session_id'), ('model_requests', 'session_id')]
    }
    # Opening --session protects this source from retention before loading its
    # fresh context. That maintenance clock is not a conversation mutation.
    activity = next(row[0] for row in db.execute('PRAGMA table_info(sessions)')
                    if row[1] == 'last_activity_ms')
    state['sessions'] = [row[:activity] + row[activity+1:] for row in state['sessions']]
    state['request_observations'] = db.execute(
        'SELECT o.* FROM request_observations o JOIN model_requests r ON r.id=o.request_id '
        'WHERE r.session_id=? ORDER BY o.completion_entry_id', (source,)).fetchall()
    return state


@dataclass(frozen=True)
class FixtureRequest:
    request_id: int
    admission_entry_id: int


def admit_fixture_request(db: sqlite3.Connection, append: Callable[..., int], *,
                          source: str, model: str, cutoff: int, now: int,
                          purpose: str, delivered: tuple[int, ...] = ()) -> FixtureRequest:
    """Stage a running request and its admission pointer in the caller's transaction."""
    request = db.execute(
        'INSERT INTO model_requests(session_id,actor_id,event_cutoff,purpose,model_json,'
        "status,delivered_events_json,created_ms) VALUES(?,'main',?,?,?,'running',?,?)",
        (source, cutoff, purpose, model, json.dumps(delivered), now)).lastrowid
    admission = append({'type': 'request_admitted', 'request_id': request,
                        'purpose': purpose, 'event_cutoff': cutoff})
    db.execute('UPDATE model_requests SET admission_entry_id=? WHERE id=?', (admission, request))
    return FixtureRequest(request, admission)


def finish_fixture_request(db: sqlite3.Connection, append: Callable[..., int],
                           request: FixtureRequest, *, status: str) -> None:
    """Stage a no-usage completion audit and matching immutable observation."""
    assert status in ('completed', 'failed', 'interrupted'), status
    completion = append({'type': 'request_finished', 'request_id': request.request_id,
                         'status': status, 'attempts': [{'status': status}]})
    db.execute('UPDATE model_requests SET status=? WHERE id=?', (status, request.request_id))
    db.execute(
        'INSERT INTO request_observations(completion_entry_id,request_id,admission_entry_id,'
        'system_prompt_entry_id,status,usage_json,checkpoint_json) '
        "SELECT ?,id,admission_entry_id,system_prompt_entry_id,?,NULL,"
        "json_extract(input_json,'$.input_estimate') FROM model_requests WHERE id=?",
        (completion, status, request.request_id))


def measure_seed_completion(db: sqlite3.Connection, source: str) -> int:
    """Change only stopped private-fixture usage, retaining the CLI's genuine input."""
    request, completion, admission, checkpoint, raw = db.execute(
        'SELECT r.id,o.completion_entry_id,o.admission_entry_id,o.checkpoint_json,e.content_json '
        'FROM request_observations o JOIN model_requests r ON r.id=o.request_id '
        'JOIN entries e ON e.id=o.completion_entry_id '
        "WHERE r.session_id=? AND r.actor_id='main' AND r.purpose='coding' "
        "AND o.status='completed' ORDER BY o.completion_entry_id DESC LIMIT 1", (source,)).fetchone()
    assert admission and checkpoint, 'Seed CLI must persist genuine admitted input accounting'
    assert db.execute(
        "SELECT admission_entry_id,json_extract(input_json,'$.input_estimate') "
        'FROM model_requests WHERE id=?', (request,)).fetchone() == (admission, checkpoint)
    usage = {'input_tokens': 10000, 'output_tokens': 32}
    audit = json.loads(raw)
    attempt = next(item for item in reversed(audit['attempts']) if item['status'] == 'completed')
    attempt['usage'] = usage
    db.execute('UPDATE entries SET content_json=? WHERE id=?', (json.dumps(audit), completion))
    db.execute('UPDATE request_observations SET usage_json=? WHERE completion_entry_id=?',
               (json.dumps(usage), completion))
    return request


def verify_selected_usage(db: sqlite3.Connection, tip: int, delivery: FixtureRequest,
                          measured_request: int) -> None:
    """Mirror MainUsage's selected-completion ordering before compaction cuts the anchor."""
    rows = db.execute(
        'WITH RECURSIVE ancestry(id,parent_id) AS ('
        'SELECT id,parent_id FROM entries WHERE id=? UNION ALL '
        'SELECT e.id,e.parent_id FROM entries e JOIN ancestry a ON e.id=a.parent_id) '
        'SELECT o.request_id,o.usage_json,o.admission_entry_id,o.checkpoint_json '
        'FROM ancestry a JOIN entries e ON e.id=a.id '
        'JOIN request_observations o ON o.completion_entry_id=coalesce(e.source_id,e.id) '
        'JOIN model_requests r ON r.id=o.request_id '
        "WHERE r.actor_id='main' AND r.purpose='coding' AND o.status='completed' "
        'ORDER BY e.id DESC', (tip,)).fetchall()
    assert rows[0][:3] == (delivery.request_id, None, delivery.admission_entry_id), rows
    anchor = next(row for row in rows if row[1] is not None)
    assert anchor[0] == measured_request and anchor[2] and anchor[3], anchor
    assert json.loads(anchor[1]) == {'input_tokens': 10000, 'output_tokens': 32}, anchor


def seed_boundary(db, status):
    source, tip, model = db.execute(
        'SELECT id,active_entry_id,model_json FROM sessions').fetchone()
    main_turn = db.execute(
        "SELECT turn_id FROM model_requests WHERE purpose='coding'").fetchone()[0]
    assert db.execute('SELECT status FROM turns WHERE id=?', (main_turn,)).fetchone() == ('completed',)
    measured_request = measure_seed_completion(db, source)
    now = int(time.time()*1000)
    child = 'main/child_fixture'
    child_turn = 'ct_fixture'
    answer = 'Storage verified; 35 fixture tests passed.'
    db.execute('INSERT INTO turns(id,session_id,actor_id,trigger,status,model_json,started_ms,finished_ms) '
               "VALUES(?,?,?,'child','completed',?,?,?)",
               (child_turn, source, child, model, now, now))

    def append(body, actor='main', kind='status', role=None, turn=None):
        nonlocal tip
        tip = db.execute(
            'INSERT INTO entries(session_id,parent_id,turn_id,actor_id,kind,role,model_visible,'
            'content_json,created_ms) VALUES(?,?,?,?,?,?,0,?,?)',
            (source, tip, turn, actor, kind, role, json.dumps(body), now)).lastrowid
        return tip

    result = append({'type': 'assistant', 'content': answer}, actor=child,
                    kind='message', role='assistant', turn=child_turn)
    finish = append({'type': 'child_turn_finished', 'child_id': child,
                     'child_turn_id': child_turn, 'job_id': 'job_child_fixture',
                     'outcome': 'completed', 'context_state': 'unknown', 'persistent': True,
                     'answer': answer, 'result_entry_id': result}, actor=child, turn=child_turn)
    pending = append({'type': 'runtime_event', 'body': {
        'type': 'job_exit', 'job_id': 'job_pending_fixture', 'exit_code': 0,
        'message': 'Durable offline job finished.'}})
    delivered = append({'type': 'runtime_event', 'body': {
        'type': 'job_exit', 'job_id': 'job_already_delivered_fixture', 'exit_code': 0}})
    delivery_request = admit_fixture_request(
        db, append, source=source, model=model, cutoff=tip, now=now,
        purpose='coding', delivered=(delivered,))
    db.execute('UPDATE entries SET delivered_request_id=? WHERE id=?',
               (delivery_request.request_id, delivered))
    finish_fixture_request(db, append, delivery_request, status='completed')
    # These are durable history only, not process/child state to restore.
    append({'type': 'job_state', 'job_id': 'job_stale_fixture', 'status': 'running'})
    append({'type': 'child_state', 'child_id': child, 'status': 'idle'})
    request = admit_fixture_request(db, append, source=source, model=model, cutoff=tip,
                                    now=now, purpose='compaction')
    if status == 'failed':
        finish_fixture_request(db, append, request, status=status)
    db.execute('UPDATE sessions SET active_entry_id=? WHERE id=?', (tip, source))
    verify_selected_usage(db, tip, delivery_request, measured_request)
    assert db.execute('SELECT count(*) FROM request_observations WHERE request_id=?',
                      (request.request_id,)).fetchone() == (int(status == 'failed'),)
    db.commit()
    assert not db.execute('PRAGMA foreign_key_check').fetchall()
    return source, {finish: 'child_turn_finished', pending: 'job_exit'}, delivered, result, answer


def verify_compaction_context(db, session, summary):
    """Check the canonical handoff separately from the raw assistant audit reply."""
    rows = db.execute(
        'SELECT role,content_json FROM entries WHERE session_id=? AND model_visible=1 ORDER BY id',
        (session,)).fetchall()
    messages = [(role, json.loads(raw)) for role, raw in rows]
    assert messages and messages[0][0] == 'developer', messages[:1]
    context = messages[0][1]
    assert context['type'] == 'compaction_context', context
    assert sum(message['type'] == 'compaction_context' for _, message in messages) == 1, messages
    assert not any(role == 'assistant' and message.get('content') in (summary, context['content'])
                   for role, message in messages), 'Summary leaked into ordinary assistant history'
    hint = ('The following is a summary of the conversation so far. '
            'Use it to continue the active task.')
    assert context['content'] == f'{hint}\n\n{summary}', context


def verify_recovery(db, source, before, wanted, delivered, result, answer):
    assert source_state(db, source) == before, 'Recovery changed source history'
    recovered = db.execute(
        "SELECT id,session_id,source_id,delivered_request_id,content_json FROM entries "
        "WHERE source_id IS NULL AND json_extract(content_json,'$.recovered_from_event_seq') IS NOT NULL"
    ).fetchall()
    assert len(recovered) == len(wanted), recovered
    originals = {json.loads(row[4])['recovered_from_event_seq'] for row in recovered}
    assert originals == set(wanted) and delivered not in originals, originals
    ack_ids = {row[3] for row in recovered}
    assert len(ack_ids) == 1 and None not in ack_ids, recovered
    request = ack_ids.pop()
    session, purpose, status, events = db.execute(
        'SELECT session_id,purpose,status,delivered_events_json FROM model_requests WHERE id=?',
        (request,)).fetchone()
    assert (purpose, status) == ('coding', 'completed'), (purpose, status)
    assert sorted(json.loads(events)) == sorted(row[0] for row in recovered), events
    for seq, owner, source_id, ack, raw in recovered:
        event = json.loads(raw)
        body = event['body']
        original = event['recovered_from_event_seq']
        assert owner != source and source_id is None and seq != original and ack == request
        assert body['type'] == wanted[original] and 'event_seq' not in body, body
        assert db.execute('SELECT delivered_request_id FROM entries WHERE id=?', (original,)).fetchone() == (None,)
        if body['type'] == 'child_turn_finished':
            assert body['answer'] == answer and body['result_entry_id'] == result, body
            assert body['outcome'] == 'completed' and 'status' not in body, body
            assert body['context_state'] == 'unknown', 'Unknown stored completion state must not be guessed'
        admitted = db.execute(
            "SELECT content_json FROM entries WHERE session_id=? AND role='user' AND source_id IS NULL "
            "AND json_extract(content_json,'$.event_seq')=?", (session, seq)).fetchall()
        assert len(admitted) == 1, admitted
        projected = json.loads(json.loads(admitted[0][0])['content'])
        assert projected['event_seq'] == seq, projected
        assert projected['recovered_from_event_seq'] == original, projected
        if body['type'] == 'child_turn_finished':
            assert projected['assignment_status'] == body['outcome'], projected
            assert projected['answer'] == answer and projected['result_entry_id'] == result, projected
            assert 'child_state' not in projected and 'context_state' not in projected, projected
            assert 'outcome' not in projected, projected
        else:
            assert projected == {**body, 'event_seq': seq, 'recovered_from_event_seq': original}, projected
    contexts = db.execute(
        "SELECT content_json FROM entries WHERE session_id=? AND role='developer' AND source_id IS NULL "
        "AND json_valid(json_extract(content_json,'$.content'))",
        (session,)).fetchall()
    context = next(json.loads(json.loads(row[0])['content']) for row in contexts
                   if json.loads(json.loads(row[0])['content']).get('type') == 'runtime_context')
    assert context['live_jobs'] == [] and context['children'] == [] and context['live_timers'] == [], context
    # The interrupted source request survives; the fresh snapshot compacts once.
    assert db.execute("SELECT count(*) FROM model_requests WHERE purpose='compaction' AND status='completed'").fetchone() == (1,)
    summary_messages = [json.loads(row[0]) for row in db.execute(
        "SELECT e.content_json FROM entries e JOIN model_requests r "
        "ON r.id=json_extract(e.content_json,'$.request_id') "
        "WHERE r.purpose='compaction' AND r.status='completed' "
        "AND json_extract(e.content_json,'$.type')='request_message' ORDER BY e.id")]
    directive = summary_messages[-2]
    assert directive['role'] == 'developer' and directive['message']['type'] == 'runtime_instruction', directive
    instruction = (Path(__file__).resolve().parents[1] / 'prompt' / 'compaction.md').read_text()
    assert directive['message']['content'] == instruction, directive
    assert summary_messages[-1]['role'] == 'assistant', summary_messages[-1]
    assert summary_messages[-1]['message']['type'] == 'assistant', summary_messages[-1]
    verify_compaction_context(db, session, summary_messages[-1]['message']['content'])
    assert any(item['role'] == 'assistant' and 'Seed complete.' in item['message'].get('content', '')
               for item in summary_messages[:-2]), 'Summary request lost structured historical evidence'
    predecessor, source_tip = db.execute(
        'SELECT predecessor_id,source_tip_id FROM compactions WHERE continuation_id=?', (session,)).fetchone()
    assert db.execute('SELECT session_id FROM entries WHERE id=?', (source_tip,)).fetchone() == (predecessor,)
    return session


def run_case(root, mode, status, binary):
    root.mkdir(mode=0o700)
    project = root / 'project'
    project.mkdir(mode=0o700)
    data = root / 'data'
    script = root / 'seed.json'
    # With the scripted 32768-token budget, exceed coding's proactive trigger
    # but leave room for the structured prefix, fixed system/tools and summary
    # directive/output. The oversized completed cycle must be summarized.
    script.write_text(json.dumps([{'prefix': 'user: Seed completed history',
                                  'text': 'Earlier evidence. ' * 3500 + '\nSeed complete.'}]))
    base = [binary, '--data-dir', str(data), '--workdir', str(project), '--auto-name=false']
    terminal = None
    try:
        terminal = Terminal(base + ['--offline-script', str(script), '--plain'], root / 'seed-terminal.log')
        terminal.expect('/help')
        terminal.send(b'Seed completed history\n')
        terminal.expect('Seed complete.')
        terminal.expect('Turn complete')
        terminal.stop(b'/quit\n')
        terminal.close()
        terminal = None
        with sqlite3.connect(data / 'history.sqlite') as db:
            db.execute('PRAGMA foreign_keys=ON')
            source, wanted, delivered, result, answer = seed_boundary(db, status)
            before = source_state(db, source)
            (root / 'source-before.json').write_text(json.dumps(before, indent=2))
        script = root / 'resume.json'
        script.write_text(json.dumps([
            # Prefix matching skips the final developer directive; inspect the
            # durable structured request in verify_recovery instead.
            {'text': '## Offline checkpoint\nEarlier evidence summarized.'},
            {'prefix': 'user: {', 'text': 'Automatic recovery continuation verified.'},
        ]))
        command = base + ['--offline-script', str(script)]
        if mode == 'session':
            command += ['--session', source]
        terminal = Terminal(command, root / 'resume-terminal.log')
        if mode == 'load':
            terminal.expect('/help')
            terminal.send(f'/load {source}\r'.encode())
        # No human prompt is sent: restored pending events start the turn.
        terminal.expect('Automatic recovery continuation verified.')
        terminal.expect('Turn complete')
        terminal.stop()
        terminal.close()
        terminal = None
        with sqlite3.connect(data / 'history.sqlite') as db:
            (root / 'source-after.json').write_text(json.dumps(source_state(db, source), indent=2))
            current = verify_recovery(db, source, before, wanted, delivered, result, answer)
            count = db.execute('SELECT count(*) FROM model_requests').fetchone()[0]
            recovered_count = db.execute("SELECT count(*) FROM entries WHERE source_id IS NULL AND json_extract(content_json,'$.recovered_from_event_seq') IS NOT NULL").fetchone()[0]
        assert not list(data.glob('lineages/*/compactions')), 'Recovery compaction eagerly wrote archives'
        idle = root / 'idle.json'
        idle.write_text('[]')  # Any unintended inference fails loudly.
        terminal = Terminal(base + ['--offline-script', str(idle), '--session', current],
                            root / 'reload-terminal.log')
        terminal.expect('/help')
        terminal.read_for(1)
        terminal.stop()
        terminal.close()
        terminal = None
        with sqlite3.connect(data / 'history.sqlite') as db:
            assert source_state(db, source) == before
            assert db.execute('SELECT count(*) FROM model_requests').fetchone() == (count,), 'Reload repeated inference'
            assert db.execute("SELECT count(*) FROM entries WHERE source_id IS NULL AND json_extract(content_json,'$.recovered_from_event_seq') IS NOT NULL").fetchone() == (recovered_count,), 'Reload recreated pending events'
        print(f'PASS: {mode}, {status} boundary, automatic compaction/continuation and idle reload; artifacts: {root}', flush=True)
    finally:
        if terminal is not None:
            terminal.close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary', default='./ttc')
    args = parser.parse_args()
    binary = str(Path(args.binary).resolve())
    root = Path(tempfile.mkdtemp(prefix='pty-compaction-reload-', dir=private_scratch()))
    print(f'Artifacts: {root}', flush=True)
    try:
        for mode, label in [('session', 'startup'), ('load', 'load')]:
            for status in ('running', 'failed'):
                run_case(root / f'{label}-{status}', mode, status, binary)
    except BaseException:
        (root / 'failure.txt').write_text(traceback.format_exc())
        print(f'FAIL; complete artifacts: {root}', flush=True)
        raise


if __name__ == '__main__':
    main()

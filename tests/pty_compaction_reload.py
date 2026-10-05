#!/usr/bin/env python3
"""Recover interrupted compaction notifications through the real offline CLI/PTY.

Only a stopped CLI's private fixture database is seeded; no live history is
modified. Every phase retains its terminal transcript, including on failure.
"""
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
    """Include every column so even cursor, timestamp or request changes fail."""
    return {
        table: db.execute(f'SELECT * FROM {table} WHERE {key}=? ORDER BY id',
                          (source,)).fetchall()
        for table, key in [('sessions', 'id'), ('entries', 'session_id'),
                           ('turns', 'session_id'), ('model_requests', 'session_id')]
    }


def seed_boundary(db, status):
    source, tip, model = db.execute(
        'SELECT id,active_entry_id,model_json FROM sessions').fetchone()
    main_turn = db.execute(
        "SELECT turn_id FROM model_requests WHERE purpose='coding'").fetchone()[0]
    assert db.execute('SELECT status FROM turns WHERE id=?', (main_turn,)).fetchone() == ('completed',)
    now = int(time.time()*1000)
    child = 'main/child_fixture'
    child_turn = 'ct_fixture'
    answer = 'Storage verified; 35 fixture tests passed.'
    db.execute('INSERT INTO turns(id,session_id,actor_id,trigger,status,model_json,started_ms,finished_ms) '
               "VALUES(?,?,?,'child','completed',?,?,?)",
               (child_turn, source, child, model, now, now))

    def append(body, actor='main', kind='status', role=None, turn=None, delivered=None):
        nonlocal tip
        tip = db.execute(
            'INSERT INTO entries(session_id,parent_id,turn_id,actor_id,kind,role,model_visible,'
            'content_json,delivered_request_id,created_ms) VALUES(?,?,?,?,?,?,0,?,?,?)',
            (source, tip, turn, actor, kind, role, json.dumps(body), delivered, now)).lastrowid
        return tip

    result = append({'role': 'assistant', 'content': answer}, actor=child,
                    kind='message', role='assistant', turn=child_turn)
    finish = append({'type': 'child_turn_finished', 'child_id': child,
                     'child_turn_id': child_turn, 'job_id': 'job_child_fixture',
                     'status': 'completed', 'persistent': True,
                     'answer': answer, 'result_entry_id': result}, actor=child, turn=child_turn)
    pending = append({'type': 'runtime_event', 'body': {
        'type': 'job_exit', 'job_id': 'job_pending_fixture', 'exit_code': 0,
        'message': 'Durable offline job finished.'}})
    # Defer the FK until commit so the event can precede the request that
    # acknowledges it, without changing a request made by the seed CLI.
    db.execute('PRAGMA defer_foreign_keys=ON')
    delivery_request = db.execute('SELECT max(id)+1 FROM model_requests').fetchone()[0]
    delivered = append({'type': 'runtime_event', 'body': {
        'type': 'job_exit', 'job_id': 'job_already_delivered_fixture', 'exit_code': 0}},
        delivered=delivery_request)
    db.execute('INSERT INTO model_requests(id,session_id,actor_id,event_cutoff,purpose,model_json,status,'
               'attempts_json,delivered_events_json,created_ms) '
               "VALUES(?,?,'main',?,'coding',?,'completed','[]',?,?)",
               (delivery_request, source, tip, model, json.dumps([delivered]), now))
    append({'type': 'request_admitted', 'request_id': delivery_request, 'event_cutoff': tip})
    append({'type': 'request_finished', 'request_id': delivery_request, 'status': 'completed'})
    # These are durable history only, not process/child state to restore.
    append({'type': 'job_state', 'job_id': 'job_stale_fixture', 'status': 'running'})
    append({'type': 'child_state', 'child_id': child, 'status': 'idle'})
    request = db.execute(
        'INSERT INTO model_requests(session_id,actor_id,event_cutoff,purpose,model_json,status,'
        "attempts_json,created_ms) VALUES(?,'main',?,'compaction',?,?,'[]',?)",
        (source, tip, model, status, now)).lastrowid
    append({'type': 'request_admitted', 'request_id': request,
            'purpose': 'compaction', 'event_cutoff': tip})
    if status == 'failed':
        append({'type': 'request_finished', 'request_id': request, 'status': status})
    db.execute('UPDATE sessions SET active_entry_id=? WHERE id=?', (tip, source))
    db.commit()
    assert not db.execute('PRAGMA foreign_key_check').fetchall()
    return source, {finish: 'child_turn_finished', pending: 'job_exit'}, delivered, result, answer


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
        assert body['type'] == wanted[original] and body['event_seq'] == seq, body
        assert db.execute('SELECT delivered_request_id FROM entries WHERE id=?', (original,)).fetchone() == (None,)
        if body['type'] == 'child_turn_finished':
            assert body['answer'] == answer and body['result_entry_id'] == result, body
        admitted = db.execute(
            "SELECT content_json FROM entries WHERE session_id=? AND role='user' AND source_id IS NULL "
            "AND json_extract(content_json,'$.event_seq')=?", (session, seq)).fetchall()
        assert len(admitted) == 1 and json.loads(json.loads(admitted[0][0])['content']) == body, admitted
    contexts = db.execute(
        "SELECT content_json FROM entries WHERE session_id=? AND role='developer' AND source_id IS NULL "
        "AND json_valid(json_extract(content_json,'$.content'))",
        (session,)).fetchall()
    context = next(json.loads(json.loads(row[0])['content']) for row in contexts
                   if json.loads(json.loads(row[0])['content']).get('type') == 'runtime_context')
    assert context['live_jobs'] == [] and context['children'] == [] and context['live_timers'] == [], context
    # The interrupted source request survives; the fresh snapshot compacts once.
    assert db.execute("SELECT count(*) FROM model_requests WHERE purpose='compaction' AND status='completed'").fetchone() == (1,)
    archive = Path(db.execute('SELECT archive_path FROM compactions WHERE continuation_id=?', (session,)).fetchone()[0])
    assert archive.exists() and Path(str(archive)+'.jsonl').exists(), archive
    return session


def run_case(root, mode, status):
    root.mkdir(mode=0o700)
    project = root / 'project'
    project.mkdir(mode=0o700)
    data = root / 'data'
    script = root / 'seed.json'
    # Fits the summarizer, but exceeds coding headroom once tools and the system
    # prompt are included. Its oversized completed cycle must be summarized.
    script.write_text(json.dumps([{'prefix': 'user: Seed completed history',
                                  'text': 'Earlier evidence. ' * 4400 + '\nSeed complete.'}]))
    binary = str(Path('./ttc').resolve())
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
        script = root / 'resume.json'
        script.write_text(json.dumps([
            {'prefix': 'user: Focus:', 'text': '## Offline checkpoint\nEarlier evidence summarized.'},
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
            current = verify_recovery(db, source, before, wanted, delivered, result, answer)
            count = db.execute('SELECT count(*) FROM model_requests').fetchone()[0]
            recovered_count = db.execute("SELECT count(*) FROM entries WHERE source_id IS NULL AND json_extract(content_json,'$.recovered_from_event_seq') IS NOT NULL").fetchone()[0]
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
    root = Path(tempfile.mkdtemp(prefix='pty-compaction-reload-', dir=private_scratch()))
    print(f'Artifacts: {root}', flush=True)
    try:
        run_case(root / 'startup-running', 'session', 'running')
        run_case(root / 'load-failed', 'load', 'failed')
    except BaseException:
        (root / 'failure.txt').write_text(traceback.format_exc())
        print(f'FAIL; complete artifacts: {root}', flush=True)
        raise


if __name__ == '__main__':
    main()

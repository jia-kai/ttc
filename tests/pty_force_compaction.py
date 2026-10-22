#!/usr/bin/env python3
"""Recover oversized local context with /force-compact in a real offline PTY.

Only the stopped seed process's private fixture database is mutated. Logs,
request/context snapshots and complete exceptions survive every invocation.
"""
import argparse
import json
from pathlib import Path
import sqlite3
import tempfile
import time
import traceback

from pty_compaction_reload import Terminal, seed_boundary, source_state, verify_recovery, verify_compaction_context
from scratch import private_scratch


PREFLIGHT_ERROR = 'automatic compaction failed: compaction input exceeds'
FOCUS = 'Preserve completed child evidence and deliver pending notices exactly once.'
SUMMARY = '## Offline checkpoint\nEarlier evidence summarized; deliver the pending completion notices.'
CONTINUATION = 'Forced recovery continuation verified.'
SEED = 'Earlier evidence. ' * 8000 + '\nSeed complete.'


def snapshot(root, data, phase, terminal=None):
    """Retain all fixture tables, including full request/context bodies."""
    if terminal is not None:
        terminal.save()
    state = {'phase': phase}
    if terminal is not None:
        state['process_exit'] = terminal.process.poll()
        state['terminal_log'] = str(terminal.log)
    database = data / 'history.sqlite'
    if database.exists():
        with sqlite3.connect(f'file:{database}?mode=ro', uri=True) as db:
            tables = [row[0] for row in db.execute(
                "SELECT name FROM sqlite_master WHERE type='table' ORDER BY name")]
            state['tables'] = {}
            for table in tables:
                query = db.execute(f'SELECT * FROM "{table}" ORDER BY rowid')
                state['tables'][table] = {
                    'columns': [column[0] for column in query.description],
                    'rows': query.fetchall(),
                }
    (root / f'{phase}-state.json').write_text(json.dumps(state, indent=2))


def stop_after_expected_failure(terminal):
    """Join gracefully while allowing only the explicitly tested preflight error."""
    terminal.send(b'/quit\n')
    deadline = time.monotonic() + 10
    while terminal.process.poll() is None and time.monotonic() < deadline:
        terminal.read_for(0.05)
    terminal.read_for(0.1)
    assert terminal.process.poll() == 0, f'CLI exit status: {terminal.process.poll()}'
    text = terminal.output.decode(errors='replace')
    errors = [line for line in text.splitlines() if 'Error:' in line]
    assert len(errors) == 1 and f'Error: {PREFLIGHT_ERROR}' in errors[0], errors
    assert 'Auto-wake paused' in text, terminal.log


def static_directive():
    """Read the Markdown instruction verbatim, including its final newline."""
    asset = Path(__file__).resolve().parents[1] / 'prompt' / 'compaction.md'
    return asset.read_text()


def visible_messages(db, session):
    messages = []
    for raw, created in db.execute(
            'SELECT content_json,created_ms FROM entries WHERE session_id=? AND model_visible=1 ORDER BY id',
            (session,)):
        message = json.loads(raw)
        if message['type'] == 'user_input':
            # Store.Messages projects the immutable original commit timestamp
            # rather than trusting any timestamp supplied in content_json.
            message['input_time_ms'] = created
        messages.append(message)
    return messages


def verify_forced_request(db, source, loaded, prefix, baseline_count):
    requests = db.execute(
        """SELECT r.id,r.session_id,r.purpose,r.status,
            (SELECT json_extract(e.content_json,'$.attempts') FROM request_observations o
             JOIN entries e ON e.id=o.completion_entry_id WHERE o.request_id=r.id
             ORDER BY o.completion_entry_id DESC LIMIT 1)
            FROM model_requests r WHERE r.id>? ORDER BY r.id""",
        (baseline_count,)).fetchall()
    assert [(row[1], row[2], row[3]) for row in requests[:1]] == [
        (loaded, 'compaction', 'completed')], requests
    assert len(requests) == 2 and requests[1][2:4] == ('coding', 'completed'), requests
    assert all(len(json.loads(row[4])) == 1 for row in requests), requests
    request = requests[0][0]
    messages = [json.loads(row[0]) for row in db.execute(
        "SELECT content_json FROM entries WHERE json_extract(content_json,'$.type')='request_message' "
        "AND json_extract(content_json,'$.request_id')=? ORDER BY id", (request,))]
    assert [item['message'] for item in messages[:-3]] == prefix, 'Summary changed the original structured prefix'
    assert messages[-3]['role'] == 'developer', messages[-3]
    assert messages[-3]['message'] == {
        'type': 'runtime_instruction', 'content': f'Compaction focus: {FOCUS}'}, messages[-3]
    assert messages[-2]['role'] == 'developer', messages[-2]
    assert messages[-2]['message'] == {
        'type': 'runtime_instruction', 'content': static_directive()}, messages[-2]
    assert messages[-1]['message'] == {'type': 'assistant', 'content': SUMMARY}, messages[-1]
    assert sum(message.get('content') == SEED for message in prefix) == 1
    boundaries = [json.loads(row[0]) for row in db.execute(
        "SELECT content_json FROM entries WHERE session_id=? AND source_id IS NULL "
        "AND json_extract(content_json,'$.type')='compaction_started' ORDER BY id", (loaded,))]
    assert boundaries == [{'type': 'compaction_started', 'force': False},
                          {'type': 'compaction_started', 'force': True}], boundaries
    assert all(type(boundary['force']) is bool for boundary in boundaries), boundaries
    # Provider cache identity is not persisted by the offline transport. Check
    # the durable lineage and system snapshot, the cache fields visible here.
    sessions = db.execute('SELECT id,lineage_id FROM sessions').fetchall()
    lineage = dict(sessions)
    assert lineage[loaded] == lineage[source], sessions
    systems = [json.loads(row[0]) for row in db.execute(
        "SELECT content_json FROM entries WHERE session_id=? "
        "AND json_extract(content_json,'$.type')='system_prompt' "
        "AND json_extract(content_json,'$.request_id')=?", (loaded, request))]
    assert len(systems) == 1, systems
    seed_system = json.loads(db.execute(
        "SELECT content_json FROM entries WHERE session_id=? "
        "AND json_extract(content_json,'$.type')='system_prompt' ORDER BY id LIMIT 1",
        (source,)).fetchone()[0])
    assert Path(systems[0]['path']).read_bytes() == Path(seed_system['path']).read_bytes(), 'Summary changed the coding system prompt'
    return {'request_ids': [row[0] for row in requests], 'summary_request': request,
            'prefix_messages': len(prefix), 'oversized_assistant_bytes': len(SEED.encode()),
            'cache_lineage': lineage[loaded], 'system_snapshot': systems[0],
            'provider_cache_identity': 'not exposed by offline scripted transport'}


def run(root, binary):
    project = root / 'project'
    project.mkdir(mode=0o700)
    data = root / 'data'
    script = root / 'seed.json'
    script.write_text(json.dumps([{'prefix': 'user: Seed completed history', 'text': SEED}]))
    base = [binary, '--data-dir', str(data), '--workdir', str(project), '--auto-name=false', '--plain']
    terminal = None
    phase = 'startup'
    try:
        terminal = Terminal(base + ['--offline-script', str(script)], root / 'seed-terminal.log')
        terminal.expect('/help')
        terminal.send(b'Seed completed history\n')
        terminal.expect('Seed complete.')
        terminal.expect('Turn complete')
        terminal.stop(b'/quit\n')
        terminal.close()
        terminal = None
        phase = 'seed-stopped'
        snapshot(root, data, phase)
        with sqlite3.connect(data / 'history.sqlite') as db:
            db.execute('PRAGMA foreign_keys=ON')
            assert db.execute('SELECT purpose,status FROM model_requests').fetchall() == [('coding', 'completed')]
            source, wanted, delivered, result, answer = seed_boundary(db, status='failed')
            before = source_state(db, source)
            baseline_count = db.execute('SELECT max(id) FROM model_requests').fetchone()[0]
            # Loading intentionally hides the old live runtime projection.
            # The authored user/model history must survive byte-for-byte.
            prefix = [message for message in visible_messages(db, source)
                      if message['type'] not in ('runtime_context', 'runtime_notification',
                                                 'runtime_instruction', 'child_guidance')]
        (root / 'source-before.json').write_text(json.dumps(before, indent=2))
        phase = 'boundary-seeded'
        snapshot(root, data, phase)
        script = root / 'resume.json'
        script.write_text(json.dumps([{'text': SUMMARY}, {'text': CONTINUATION}]))
        terminal = Terminal(base + ['--offline-script', str(script), '--session', source],
                            root / 'resume-terminal.log')
        terminal.expect(PREFLIGHT_ERROR)
        terminal.expect('Auto-wake paused')
        terminal.read_for(0.5)
        phase = 'automatic-preflight-refused'
        snapshot(root, data, phase, terminal)
        with sqlite3.connect(data / 'history.sqlite') as db:
            assert source_state(db, source) == before
            assert db.execute('SELECT max(id) FROM model_requests').fetchone() == (baseline_count,), 'Automatic preflight admitted inference'
            loaded, = [row[0] for row in db.execute('SELECT id FROM sessions WHERE id!=?', (source,))]
            assert visible_messages(db, loaded) == prefix, 'Failed automatic attempt changed the original prefix'
            recovered = db.execute(
                "SELECT delivered_request_id FROM entries WHERE source_id IS NULL "
                "AND json_extract(content_json,'$.recovered_from_event_seq') IS NOT NULL").fetchall()
            assert recovered == [(None,)] * len(wanted), recovered
        terminal.send(f'/force-compact {FOCUS}\n'.encode())
        terminal.expect(CONTINUATION)
        terminal.expect('Turn complete')
        stop_after_expected_failure(terminal)
        terminal.close()
        terminal = None
        phase = 'forced-recovery-completed'
        snapshot(root, data, phase)
        with sqlite3.connect(data / 'history.sqlite') as db:
            current = verify_recovery(db, source, before, wanted, delivered, result, answer)
            evidence = verify_forced_request(db, source, loaded, prefix, baseline_count)
            assert db.execute('SELECT count(*) FROM compactions').fetchone() == (1,)
            final_messages = visible_messages(db, current)
            verify_compaction_context(db, current, SUMMARY)
            assert not any(SEED in message.get('content', '') or FOCUS in message.get('content', '')
                           for message in final_messages), 'Handoff retained oversized history or temporary focus'
            count = db.execute('SELECT count(*) FROM model_requests').fetchone()[0]
            recovered_count = db.execute(
                "SELECT count(*) FROM entries WHERE source_id IS NULL "
                "AND json_extract(content_json,'$.recovered_from_event_seq') IS NOT NULL").fetchone()[0]
        (root / 'verification.json').write_text(json.dumps(evidence, indent=2))
        assert not list(data.glob('lineages/*/compactions')), 'Recovery eagerly created archives'
        idle = root / 'idle.json'
        idle.write_text('[]')  # Any extra provider call must fail loudly.
        terminal = Terminal(base + ['--offline-script', str(idle), '--session', current],
                            root / 'reload-terminal.log')
        terminal.expect('/help')
        terminal.read_for(1)
        terminal.stop(b'/quit\n')
        terminal.close()
        terminal = None
        phase = 'idle-reload-completed'
        snapshot(root, data, phase)
        with sqlite3.connect(data / 'history.sqlite') as db:
            assert source_state(db, source) == before
            assert db.execute('SELECT count(*) FROM model_requests').fetchone() == (count,), 'Reload repeated inference'
            assert db.execute(
                "SELECT count(*) FROM entries WHERE source_id IS NULL "
                "AND json_extract(content_json,'$.recovered_from_event_seq') IS NOT NULL").fetchone() == (recovered_count,), 'Reload redelivered notifications'
        print(f'PASS: automatic refusal, forced summary/fitting continuation, exactly-once notices and idle reload; artifacts: {root}', flush=True)
    except BaseException:
        (root / 'failure.txt').write_text(traceback.format_exc())
        try:
            snapshot(root, data, f'failure-after-{phase}', terminal)
        except BaseException:
            (root / 'snapshot-failure.txt').write_text(traceback.format_exc())
        raise
    finally:
        if terminal is not None:
            terminal.close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary', default='./ttc', help='TTC executable (default: ./ttc)')
    args = parser.parse_args()
    root = Path(tempfile.mkdtemp(prefix='pty-force-compaction-', dir=private_scratch()))
    print(f'Artifacts: {root}', flush=True)
    try:
        binary = str(Path(args.binary).resolve(strict=True))
        (root / 'invocation.json').write_text(json.dumps({'binary': binary}, indent=2))
        run(root, binary)
    except BaseException:
        (root / 'failure.txt').write_text(traceback.format_exc())
        print(f'FAIL; complete artifacts: {root}', flush=True)
        raise


if __name__ == '__main__':
    main()

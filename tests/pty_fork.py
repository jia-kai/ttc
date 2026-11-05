#!/usr/bin/env python3
"""Offline PTY check: shared-prefix forks with concurrently live source instances."""
import sqlite3
import tempfile
import traceback
from pathlib import Path

from pty_parallel import Instance
from scratch import private_scratch


def selected_messages(db, session):
    return [row[0] for row in db.execute('''
        WITH RECURSIVE ancestry(id,parent_id) AS (
            SELECT e.id,e.parent_id FROM sessions s
            JOIN entries e ON e.id=s.active_entry_id WHERE s.id=?
            UNION ALL SELECT e.id,e.parent_id FROM entries e
            JOIN ancestry a ON e.id=a.parent_id
        ) SELECT json_extract(e.content_json,'$.content')
          FROM ancestry a JOIN entries e ON e.id=a.id
          WHERE e.kind='message' AND e.role='assistant' ORDER BY e.id
    ''', (session,))]


def main():
    root = Path(tempfile.mkdtemp(prefix='pty-fork-', dir=private_scratch()))
    (root / 'project').mkdir()
    print(f'Artifacts: {root}', flush=True)
    instances = []
    try:
        original = Instance(root, 'original', [
            {'text': 'Shared prefix answer'}, {'text': 'Original independent answer'}])
        instances.append(original)
        original.expect('/help')
        original.send('Establish shared prefix')
        original.expect('Shared prefix answer')
        original.expect('Turn complete')
        with sqlite3.connect(root / 'data' / 'history.sqlite') as db:
            source, source_tip = db.execute(
                'SELECT id,active_entry_id FROM sessions').fetchone()
            before = db.execute('SELECT count(*) FROM entries').fetchone()[0]

        fork = Instance(root, 'fork', [
            {'text': 'Fork independent answer'}, {'text': 'Fork later answer'}], load=source)
        instances.append(fork)
        fork.expect('/help')
        with sqlite3.connect(root / 'data' / 'history.sqlite') as db:
            fork_id, base, active = db.execute('''
                SELECT id,base_entry_id,active_entry_id FROM sessions
                WHERE base_session_id=?''', (source,)).fetchone()
            assert base == active == source_tip
            assert db.execute('SELECT count(*) FROM entries').fetchone()[0] == before
            assert db.execute('SELECT count(*) FROM entries WHERE session_id=?',
                              (fork_id,)).fetchone()[0] == 0

        # Both runtimes stay open and continue independently from the shared cut.
        fork.send('Continue the fork')
        fork.expect('Fork independent answer')
        fork.expect('Turn complete')
        original.send('Continue the original')
        original.expect('Original independent answer')
        original.expect('Turn complete')
        with sqlite3.connect(root / 'data' / 'history.sqlite') as db:
            assert selected_messages(db, source) == [
                'Shared prefix answer', 'Original independent answer']
            assert selected_messages(db, fork_id) == [
                'Shared prefix answer', 'Fork independent answer']
            fork_tip = db.execute('SELECT active_entry_id FROM sessions WHERE id=?',
                                  (fork_id,)).fetchone()[0]
            before = db.execute('SELECT count(*) FROM entries').fetchone()[0]

        nested = Instance(root, 'nested', [{'text': 'Nested independent answer'}], load=fork_id)
        instances.append(nested)
        nested.expect('/help')
        with sqlite3.connect(root / 'data' / 'history.sqlite') as db:
            nested_id, base, active = db.execute('''
                SELECT id,base_entry_id,active_entry_id FROM sessions
                WHERE base_session_id=?''', (fork_id,)).fetchone()
            assert base == active == fork_tip
            assert db.execute('SELECT count(*) FROM entries').fetchone()[0] == before
        fork.send('Advance after nested fork')
        fork.expect('Fork later answer')
        fork.expect('Turn complete')
        nested.send('Continue nested fork')
        nested.expect('Nested independent answer')
        nested.expect('Turn complete')
        with sqlite3.connect(root / 'data' / 'history.sqlite') as db:
            assert selected_messages(db, nested_id) == [
                'Shared prefix answer', 'Fork independent answer',
                'Nested independent answer']
            assert db.execute('PRAGMA foreign_key_check').fetchall() == []
        for instance in list(instances):
            instance.stop()
            instances.remove(instance)
        print('PASS: zero-copy load, live source/fork continuation, frozen nested fork')
    except BaseException:
        (root / 'failure.txt').write_text(traceback.format_exc())
        raise
    finally:
        for instance in instances:
            instance.stop(kill=True)


if __name__ == '__main__':
    main()

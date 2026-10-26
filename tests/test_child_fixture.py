"""Checks for the demo's compact child evidence assertions."""
import unittest
from pathlib import Path
import json
import sqlite3
import tempfile

from child_fixture import CHILD_ANSWER, CHILD_ANSWER_CAP, CHILD_BODY, CHILD_PREFIX, CHILD_TAIL, child_response_row, child_response_target, compact_rows, demo_completed, verify_child_answers


class ChildFixtureTests(unittest.TestCase):
    def test_long_multiline_fixture(self):
        self.assertGreater(len(CHILD_ANSWER.encode()), 8192)
        self.assertGreater(len(CHILD_ANSWER.splitlines()), 16)
        self.assertTrue(CHILD_ANSWER.startswith(CHILD_PREFIX))
        self.assertTrue(CHILD_ANSWER.endswith(CHILD_TAIL))
        self.assertNotIn(CHILD_BODY, CHILD_ANSWER[:256])

    def test_compact_rows(self):
        rows = ('header\n[Fixture check] assistant ' + CHILD_PREFIX + ' ...\n'
                'subagent · done · [Fixture check] final answer ' + CHILD_PREFIX + '…')
        self.assertEqual(compact_rows(rows), (2, 3))

        boundary = '[Fixture check] Request #7 · coding · attempt 1 · Inspect ' + CHILD_PREFIX
        self.assertEqual(compact_rows(rows + '\n' + boundary), (2, 3))

    def test_reject_expanded_or_missing_rows(self):
        valid = ('[Fixture check] assistant ' + CHILD_PREFIX + ' ...\n'
                 'subagent · done · Sub Fixture check final answer ' + CHILD_PREFIX + ' ...')
        for invalid in (valid + '\n' + CHILD_BODY, valid + '\n' + CHILD_TAIL,
                        valid + '\n{"answer": "long"}', valid.splitlines()[0],
                        valid.replace('...', '')):
            with self.subTest(invalid=invalid), self.assertRaises(AssertionError):
                compact_rows(invalid)

    def test_narrow_requires_both_clipped_rows(self):
        response = '[Fixture check] assistant CHILD…'
        completion = 'subagent · done · Sub Fixture check final…'
        self.assertEqual(compact_rows(response + '\n' + completion, narrow=True), (1, 2))
        for text in (response, completion, response + '\n' + completion + '\n' + CHILD_BODY):
            with self.subTest(text=text), self.assertRaises(AssertionError):
                compact_rows(text, narrow=True)

    def test_native_response_target_and_clipped_row(self):
        title = 'Request #7 · coding · attempt 1'
        with sqlite3.connect(':memory:') as db:
            db.execute('CREATE TABLE entries(id,actor_id,kind,content_json)')
            with self.assertRaises(AssertionError):
                child_response_target(db)
            for identifier, actor, direction, suffix in (
                    (1, 'main', 'response', 'assistant message'),
                    (2, 'main/child_fixture', 'request', 'assistant message'),
                    (3, 'main/child_fixture', 'response', 'Tool call · read'),
                    (4, 'main/child_fixture', 'response', 'assistant message')):
                db.execute('INSERT INTO entries VALUES(?,?,?,?)', (identifier, actor, 'status', json.dumps({
                    'type': 'presentation_stream', 'direction': direction, 'purpose': 'coding',
                    'item': {'title': title},
                })))
            self.assertEqual(child_response_target(db), (4, title))
        clipped = '[Fixture check] Request #7 · coding · attempt 1 · com…'
        self.assertEqual(child_response_row('header\n' + clipped, title), 2)
        for text in (clipped.replace('coding', 'naming'), clipped + '\n' + clipped):
            with self.subTest(text=text), self.assertRaises(AssertionError):
                child_response_row(text, title)

    def test_completion_requires_durable_turn_and_answer(self):
        with tempfile.TemporaryDirectory() as root:
            database = Path(root) / 'history.sqlite'
            self.assertFalse(demo_completed(database, 'notebook'))
            with sqlite3.connect(database) as db:
                db.executescript("CREATE TABLE turns(actor_id,status); CREATE TABLE entries(id,actor_id,kind,role,content_json); INSERT INTO turns VALUES('main','running'); INSERT INTO entries VALUES(1,'main','message','assistant','{\"type\":\"assistant\",\"content\":\"notebook\"}');")
            self.assertFalse(demo_completed(database, 'notebook'))
            with sqlite3.connect(database) as db:
                db.execute("UPDATE turns SET status='completed'")
            self.assertFalse(demo_completed(database, 'other notebook'))
            self.assertTrue(demo_completed(database, 'notebook'))
            with sqlite3.connect(database) as db:
                db.execute("UPDATE turns SET status='failed'")
            with self.assertRaises(AssertionError):
                demo_completed(database, 'notebook')

    def test_capped_answer_references_full_child_message(self):
        with sqlite3.connect(':memory:') as db:
            db.execute('CREATE TABLE entries(id,actor_id,content_json)')
            db.execute('INSERT INTO entries VALUES(1,?,?)',
                       ('main/child_fixture', json.dumps({'type': 'assistant', 'content': CHILD_ANSWER})))
            result = {'answer': CHILD_ANSWER[:CHILD_ANSWER_CAP],
                      'answer_truncated': True, 'result_entry_id': 1}
            verify_child_answers(db, [result])
            for invalid in (dict(result, answer=CHILD_ANSWER),
                            dict(result, answer_truncated=False),
                            dict(result, result_entry_id=2)):
                with self.subTest(result=invalid), self.assertRaises(AssertionError):
                    verify_child_answers(db, [invalid])
            for message in ({'content': CHILD_ANSWER},
                            {'type': 'user_input', 'content': CHILD_ANSWER},
                            {'type': 'assistant', 'role': 'assistant', 'content': CHILD_ANSWER},
                            {'type': 'assistant', 'runtime': False, 'content': CHILD_ANSWER}):
                db.execute('UPDATE entries SET content_json=? WHERE id=1', (json.dumps(message),))
                with self.subTest(message=message), self.assertRaises(AssertionError):
                    verify_child_answers(db, [result])


if __name__ == '__main__':
    unittest.main()

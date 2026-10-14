"""Checks for the demo's compact child evidence assertions."""
import unittest
from pathlib import Path
import json
import sqlite3
import tempfile

from child_fixture import CHILD_ANSWER, CHILD_ANSWER_CAP, CHILD_BODY, CHILD_PREFIX, CHILD_TAIL, compact_rows, demo_completed, verify_child_answers


class ChildFixtureTests(unittest.TestCase):
    def test_long_multiline_fixture(self):
        self.assertGreater(len(CHILD_ANSWER.encode()), 8192)
        self.assertGreater(len(CHILD_ANSWER.splitlines()), 16)
        self.assertTrue(CHILD_ANSWER.startswith(CHILD_PREFIX))
        self.assertTrue(CHILD_ANSWER.endswith(CHILD_TAIL))
        self.assertNotIn(CHILD_BODY, CHILD_ANSWER[:256])

    def test_compact_rows(self):
        rows = ('header\n[Sub Fixture check] assistant ' + CHILD_PREFIX + ' ...\n'
                'subagent · done · [Sub Fixture check] final answer ' + CHILD_PREFIX + '…')
        self.assertEqual(compact_rows(rows), (2, 3))

    def test_reject_expanded_or_missing_rows(self):
        valid = ('[Sub Fixture check] assistant ' + CHILD_PREFIX + ' ...\n'
                 'subagent · done · Sub Fixture check final answer ' + CHILD_PREFIX + ' ...')
        for invalid in (valid + '\n' + CHILD_BODY, valid + '\n' + CHILD_TAIL,
                        valid + '\n{"answer": "long"}', valid.splitlines()[0],
                        valid.replace('...', '')):
            with self.subTest(invalid=invalid), self.assertRaises(AssertionError):
                compact_rows(invalid)

    def test_narrow_requires_both_clipped_rows(self):
        response = '[Sub Fixture check] assistant CHILD…'
        completion = 'subagent · done · Sub Fixture check final…'
        self.assertEqual(compact_rows(response + '\n' + completion, narrow=True), (1, 2))
        for text in (response, completion, response + '\n' + completion + '\n' + CHILD_BODY):
            with self.subTest(text=text), self.assertRaises(AssertionError):
                compact_rows(text, narrow=True)

    def test_completion_requires_durable_turn_and_answer(self):
        with tempfile.TemporaryDirectory() as root:
            database = Path(root) / 'history.sqlite'
            self.assertFalse(demo_completed(database, 'notebook'))
            with sqlite3.connect(database) as db:
                db.executescript("CREATE TABLE turns(actor_id,status); CREATE TABLE entries(id,actor_id,kind,role,content_json); INSERT INTO turns VALUES('main','running'); INSERT INTO entries VALUES(1,'main','message','assistant','{\"content\":\"notebook\"}');")
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
                       ('main/child_fixture', json.dumps({'content': CHILD_ANSWER})))
            result = {'answer': CHILD_ANSWER[:CHILD_ANSWER_CAP],
                      'answer_truncated': True, 'result_entry_id': 1}
            verify_child_answers(db, [result])
            for invalid in (dict(result, answer=CHILD_ANSWER),
                            dict(result, answer_truncated=False),
                            dict(result, result_entry_id=2)):
                with self.subTest(result=invalid), self.assertRaises(AssertionError):
                    verify_child_answers(db, [invalid])


if __name__ == '__main__':
    unittest.main()

"""Fixture diagnostics must surface failures instead of waiting for a UI timeout."""
import json
from pathlib import Path
import re
import sqlite3
import subprocess
import sys
import tempfile
import unittest

from scratch import private_scratch
from pty_compaction import export_responses, verify_exports


class CompactionExportTest(unittest.TestCase):
    def test_script_uses_scratch_targets_and_existing_read_tools(self):
        root = Path('/tmp/fixture')
        responses = export_responses(root)
        calls = [call for response in responses for call in response.get('calls', [])]
        self.assertEqual([call['name'] for call in calls],
                         ['conversation_export', 'conversation_export', 'grep', 'read', 'conversation_export'])
        first, exact, _, _, repeated = calls
        self.assertEqual(first['arguments'], repeated['arguments'])
        self.assertEqual(exact['arguments']['format'], 'jsonl')
        self.assertEqual(exact['arguments']['scope'], 'before_compaction')
        for call in calls:
            self.assertEqual(Path(call['arguments']['path']).parent, root)

    def test_export_verification_checks_contents_and_actual_tool_results(self):
        with tempfile.TemporaryDirectory(dir=private_scratch()) as directory, sqlite3.connect(':memory:') as db:
            root = Path(directory)
            original = b'Growth 1 complete.\n'
            (root / 'history.md').write_bytes(original)
            (root / 'before-compaction.jsonl').write_text(json.dumps({'content': original.decode()}) + '\n')
            db.execute('CREATE TABLE tool_calls(provider_call_id,result_json)')
            for call in ('export-markdown', 'export-exact', 'lookup-original', 'read-export', 'reject-existing'):
                db.execute('INSERT INTO tool_calls VALUES(?,?)', (call, json.dumps({
                    'ok': call != 'reject-existing', 'content': original.decode(),
                })))
            verify_exports(db, root, original)
            (root / 'history.md').write_bytes(b'overwritten')
            with self.assertRaisesRegex(AssertionError, 'changed exported contents'):
                verify_exports(db, root, original)
            (root / 'history.md').write_bytes(original)
            db.execute("UPDATE tool_calls SET result_json=? WHERE provider_call_id='reject-existing'",
                       (json.dumps({'ok': True}),))
            with self.assertRaises(AssertionError):
                verify_exports(db, root, original)


class CompactionDiagnosticsTest(unittest.TestCase):
    def run_fixture(self, binary):
        completed = subprocess.run(
            [sys.executable, str(Path(__file__).with_name('pty_compaction.py')),
             '--binary', str(binary)], capture_output=True, text=True, timeout=5)
        self.assertNotEqual(completed.returncode, 0)
        match = re.search(r'FAIL; complete artifacts: (.+)', completed.stdout)
        self.assertIsNotNone(match, completed.stdout + completed.stderr)
        root = Path(match[1])
        self.assertTrue((root / 'terminal.log').exists())
        return root, json.loads((root / 'state.json').read_text()), (root / 'failure.txt').read_text()

    def test_early_exit_has_actual_status(self):
        _, state, failure = self.run_fixture('/bin/false')
        self.assertEqual(state['exit_status'], 1)
        self.assertIn('TTC exited with 1', failure)

    def test_durable_failure_surfaces_request_error(self):
        with tempfile.TemporaryDirectory(prefix='pty-compaction-diagnostics-', dir=private_scratch()) as directory:
            binary = Path(directory) / 'fake-ttc'
            binary.write_text(f'#!{sys.executable}\n' + '''
import json, pathlib, sqlite3, sys, time
root = pathlib.Path(sys.argv[sys.argv.index('--data-dir') + 1])
root.mkdir()
with sqlite3.connect(root / 'history.sqlite') as db:
    db.executescript("""
CREATE TABLE turns(id,status,started_ms);
INSERT INTO turns VALUES('turn','failed',0);
CREATE TABLE model_requests(id,purpose,status);
CREATE TABLE request_observations(completion_entry_id,request_id);
CREATE TABLE entries(id INTEGER PRIMARY KEY,content_json);
""")
    db.execute('INSERT INTO model_requests VALUES(1,?,?)', ('coding', 'failed'))
    db.execute('INSERT INTO entries(content_json) VALUES(?)',
               (json.dumps({'error': 'original fixture failure', 'attempts': [{'error': 'original fixture failure'}]}),))
    db.execute('INSERT INTO request_observations VALUES(1,1)')
print('/help', flush=True)
time.sleep(30)
''')
            binary.chmod(0o700)
            _, state, failure = self.run_fixture(binary)
            self.assertIsNone(state['exit_status'])
            self.assertEqual(state['cleanup_exit_status'], -9)
            self.assertIn('TTC failed before rendering', failure)
            self.assertIn('original fixture failure', failure)
            self.assertEqual(state['history']['turns'], [['turn', 'failed']])


if __name__ == '__main__':
    unittest.main()

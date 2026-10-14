"""Fixture diagnostics must surface failures instead of waiting for a UI timeout."""
import json
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import unittest

from scratch import private_scratch


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
CREATE TABLE model_requests(id,purpose,status,attempts_json);
CREATE TABLE entries(id INTEGER PRIMARY KEY,content_json);
""")
    db.execute('INSERT INTO model_requests VALUES(1,?,?,?)',
               ('coding', 'failed', json.dumps([{'error': 'original fixture failure'}])))
    db.execute('INSERT INTO entries(content_json) VALUES(?)',
               (json.dumps({'error': 'original fixture failure'}),))
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

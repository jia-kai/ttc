#!/usr/bin/env python3
"""Headless partial-stream recovery with synthetic auth and a local SSE server."""
import argparse
import json
import os
from pathlib import Path
import pty
import select
import signal
import sqlite3
import subprocess
import tempfile
import threading
import time
import traceback
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from scratch import private_scratch


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary', default='./ttc')
    args = parser.parse_args()
    root = Path(tempfile.mkdtemp(prefix='pty-partial-retry-', dir=private_scratch()))
    project = root / 'project'
    project.mkdir()
    result = project / 'result.txt'
    result.write_text('before\n')
    auth = root / 'auth.json'
    auth.write_text(json.dumps({'auth_mode': 'chatgpt', 'tokens': {
        'access_token': 'mock-token', 'account_id': 'mock-account'}}))
    auth.chmod(0o600)
    requests, errors = [], []

    class Mock(BaseHTTPRequestHandler):
        def log_message(self, *unused):
            pass

        def do_GET(self):
            assert self.path.startswith('/models?')
            self.send_response(200)
            self.send_header('Content-Type', 'application/json')
            self.end_headers()
            self.wfile.write(json.dumps({'models': [{
                'slug': 'mock-model', 'display_name': 'Mock', 'visibility': 'list',
                'context_window': 100000, 'default_reasoning_level': 'low',
                'supported_reasoning_levels': [{'effort': 'low'}]}]}).encode())

        def do_POST(self):
            try:
                assert self.path == '/responses'
                assert self.headers['Authorization'] == 'Bearer mock-token'
                body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
                requests.append(body)
                (root / 'requests.json').write_text(json.dumps(requests, indent=2))
                step = len(requests)
                self.send_response(200)
                self.send_header('Content-Type', 'text/event-stream')
                self.end_headers()

                def frame(value):
                    self.wfile.write(('data: ' + json.dumps(value) + '\n\n').encode())
                    self.wfile.flush()

                if step == 1:
                    arguments = json.dumps({'path': 'result.txt', 'old_text': 'before\n', 'new_text': 'after\n'})
                    item = {'type': 'function_call', 'id': 'item_edit', 'call_id': 'edit_once', 'name': 'edit', 'arguments': ''}
                    frame({'type': 'response.output_item.added', 'output_index': 0, 'item': item})
                    frame({'type': 'response.function_call_arguments.delta', 'output_index': 0, 'item_id': 'item_edit', 'delta': arguments})
                    frame({'type': 'response.function_call_arguments.done', 'output_index': 0, 'item_id': 'item_edit', 'arguments': arguments})
                    frame({'type': 'response.output_item.done', 'output_index': 0, 'item': dict(item, arguments=arguments, status='completed')})
                elif step == 2:
                    assert result.read_text() == 'after\n'
                    frame({'type': 'response.output_text.delta', 'delta': 'Edits saved; checking the result.'})
                    frame({'type': 'response.output_item.added', 'output_index': 0, 'item': {
                        'type': 'function_call', 'id': 'item_unfinished', 'call_id': 'unfinished',
                        'name': 'shell', 'arguments': ''}})
                    # EOF after partial text and an announcement, without completion.
                    return
                elif step == 3:
                    assert result.read_text() == 'after\n'
                    warnings = [item for item in body['input'] if item.get('role') == 'developer'
                                and 'TTC is retrying' in item['content'][0]['text']]
                    assert len(warnings) == 1, 'missing or duplicate recovery warning'
                    assert 'Reissued tool calls may fail' in warnings[0]['content'][0]['text']
                    assert any(item.get('role') == 'assistant' and
                               'Edits saved' in item['content'][0]['text'] for item in body['input'])
                    outputs = [item for item in body['input'] if item['type'] == 'function_call_output']
                    assert len(outputs) == 1 and outputs[0]['call_id'] == 'edit_once'
                    assert json.loads(outputs[0]['output'])['ok']
                    assert not any(item.get('call_id') == 'unfinished' for item in body['input'])
                    text = 'Recovery verified without repeating the edit.'
                    frame({'type': 'response.output_text.delta', 'delta': text})
                    frame({'type': 'response.output_item.done', 'output_index': 0, 'item': {
                        'type': 'message', 'id': 'message_final', 'role': 'assistant', 'status': 'completed',
                        'content': [{'type': 'output_text', 'text': text, 'annotations': []}]}})
                else:
                    raise AssertionError(f'unexpected request {step}')
                frame({'type': 'response.completed', 'response': {'id': f'response_{step}'}})
            except Exception:
                errors.append(traceback.format_exc())
                (root / 'server-errors.log').write_text('\n'.join(errors))

    server = ThreadingHTTPServer(('127.0.0.1', 0), Mock)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    master, slave = pty.openpty()
    process = None
    output = bytearray()
    cursor = 0

    def expect(needle, timeout=15):
        nonlocal cursor
        deadline = time.monotonic() + timeout
        while True:
            pos = output.find(needle.encode(), cursor)
            if pos >= 0:
                cursor = pos + len(needle.encode())
                return
            if time.monotonic() >= deadline:
                raise AssertionError(f'timed out waiting for {needle!r}; artifacts: {root}')
            if select.select([master], [], [], 0.1)[0]:
                try:
                    data = os.read(master, 65536)
                except OSError:
                    data = b''
                if not data:
                    raise AssertionError(f'PTY exited waiting for {needle!r}; artifacts: {root}')
                output.extend(data)
                (root / 'transcript.log').write_bytes(output)

    try:
        process = subprocess.Popen([
            str(Path(args.binary).resolve()), '--plain', '--workdir', str(project),
            '--data-dir', str(root / 'data'), '--openai-base-url', f'http://127.0.0.1:{server.server_port}',
            '--import-codex-auth', str(auth), '--model', 'mock-model', '--variant', 'low', '--auto-name=false',
        ], stdin=slave, stdout=slave, stderr=slave, start_new_session=True)
        os.close(slave)
        slave = -1
        os.write(master, b'Edit once, then verify the result.\n')
        expect('Retrying · attempt 2')
        expect('Recovery verified without repeating the edit.')
        expect('Turn complete')
        assert not errors, errors
        assert len(requests) == 3
        assert result.read_text() == 'after\n'
        with sqlite3.connect(root / 'data/history.sqlite') as db:
            rows = db.execute("SELECT id,status,attempts_json FROM model_requests WHERE purpose='coding' ORDER BY id").fetchall()
            assert [row[1] for row in rows] == ['completed', 'failed', 'completed'], rows
            assert 'interrupted' in json.loads(rows[1][2])[0]['error']
            assert db.execute('SELECT count(*) FROM tool_calls').fetchone()[0] == 1
            warning, visible = db.execute("SELECT content_json,model_visible FROM entries WHERE role='developer' AND json_extract(content_json,'$.content') LIKE '%TTC is retrying%'").fetchone()
            warning = json.loads(warning)
            assert visible and warning['runtime'] and warning['request_id'] == rows[1][0]
            assert db.execute("SELECT count(*) FROM turns WHERE actor_id='main' AND status='completed'").fetchone()[0] == 1
        os.write(master, b'/undo\n')
        expect('undo completed')
        assert result.read_text() == 'before\n', 'recovery split the original undo boundary'
        os.write(master, b'/quit\n')
        process.wait(timeout=5)
        assert process.returncode == 0
        print(f'PASS: partial-stream continuation, recovery prompt, one edit, interrupted announcement, undo; artifacts: {root}')
    except Exception:
        (root / 'failure.log').write_text(traceback.format_exc())
        raise
    finally:
        if process is not None and process.poll() is None:
            os.killpg(process.pid, signal.SIGTERM)
            process.wait(timeout=5)
        if slave >= 0:
            os.close(slave)
        os.close(master)
        server.shutdown()
        server.server_close()
        thread.join(timeout=5)


if __name__ == '__main__':
    main()

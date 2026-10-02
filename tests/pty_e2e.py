#!/usr/bin/env python3
"""Linux PTY integration against a local mock OpenAI server; synthetic auth, no live inference."""
import argparse
import json
import os
import pathlib
import pty
import select
import signal
import sqlite3
import subprocess
import tempfile
import time
import traceback
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


from scratch import private_scratch

def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary', default='./ttc')
    args = parser.parse_args()
    scratch = private_scratch()
    root = pathlib.Path(tempfile.mkdtemp(prefix='pty-e2e-', dir=scratch))
    workspace = root / 'workspace'
    workspace.mkdir()
    responses = [
        {'prefix': 'user: Create', 'calls': [
            {'id': 'write_1', 'name': 'write', 'arguments': {'path': 'result.txt', 'content': 'verified\n'}}]},
        {'prefix': 'tool: ', 'text': 'Created scripted result.'},
        {'prefix': 'user: Parallel', 'text': 'Running independent tools.', 'calls': [
            {'id': 'write_parallel_1', 'name': 'write', 'arguments': {'path': 'ordered.txt', 'content': 'first\n'}},
            {'id': 'parallel_left', 'name': 'shell', 'arguments': {'command': 'touch left.ready; for i in $(seq 1 200); do test -f right.ready && printf left-overlapped && { while ! test -f batch.release; do sleep 0.01; done; exit 0; }; sleep 0.01; done; exit 42'}},
            {'id': 'parallel_read', 'name': 'read', 'arguments': {'path': 'result.txt'}},
            {'id': 'write_parallel_2', 'name': 'write', 'arguments': {'path': 'ordered.txt', 'content': 'second\n'}},
            {'id': 'parallel_right', 'name': 'shell', 'arguments': {'command': 'touch right.ready; for i in $(seq 1 200); do test -f left.ready && printf right-overlapped && { while ! test -f batch.release; do sleep 0.01; done; exit 0; }; sleep 0.01; done; exit 42'}}]},
        {'prefix': 'tool: ', 'text': 'Parallel batch verified.'},
        {'prefix': 'user: Background', 'calls': [
            {'id': 'shell_1', 'name': 'shell', 'arguments': {'command': 'sleep 0.3; printf background-complete', 'background': True}}]},
        {'prefix': 'tool: ', 'text': 'Background job launched.'},
        {'prefix': 'user: {"type":"job_exit"', 'text': 'Background completion observed.'},
        {'prefix': 'user: Long job', 'calls': [
            {'id': 'shell_2', 'name': 'shell', 'arguments': {'command': 'sleep 30', 'background': True, 'wake_on_exit': False}}]},
        {'prefix': 'tool: ', 'text': 'Long job started.'},
    ]
    requests = []
    server_errors = []
    request_lock = threading.Lock()
    failed_request = None

    class MockOpenAI(BaseHTTPRequestHandler):
        def log_message(self, *args):
            pass

        def do_GET(self):
            if not self.path.startswith('/models?'):
                self.send_error(404)
                return
            body = json.dumps({'models': [{'slug': 'mock-model', 'display_name': 'Mock OpenAI',
                'visibility': 'list', 'service_tiers': [{'id': 'priority', 'name': 'Fast', 'description': 'Mock Fast tier'}],
                'context_window': 100000, 'default_reasoning_level': 'low',
                'supported_reasoning_levels': [{'effort': 'low'}], 'input_modalities': ['text']}]})
            self.send_response(200)
            self.send_header('Content-Type', 'application/json')
            self.end_headers()
            self.wfile.write(body.encode())

        def do_POST(self):
            nonlocal failed_request
            if self.path != '/responses':
                self.send_error(404)
                return
            body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
            with request_lock:
                if failed_request is None:
                    failed_request = body
                    self.send_response(503)
                    self.send_header('Retry-After', '0')
                    self.end_headers()
                    return
                index = len(requests)
                requests.append(body)
            try:
                assert self.headers['Authorization'] == 'Bearer mock-token'
                assert self.headers['ChatGPT-Account-ID'] == 'mock-account'
                assert body['store'] is False and body['stream'] is True
                assert body['parallel_tool_calls'] is True
                assert body['model'] == 'mock-model'
                expected_tier = 'default' if index < 3 else 'priority'
                assert body['service_tier'] == expected_tier
                assert self.headers['x-codex-routing-hint'] == 'model=mock-model;tier=' + expected_tier
                assert 'max_output_tokens' not in body
                for definition in body['tools']:
                    assert isinstance(definition['parameters']['properties'], dict)
                    assert isinstance(definition['parameters']['required'], list)
                response = responses[index]
                if response.get('text') == 'Parallel batch verified.':
                    results = {item['call_id']: json.loads(item['output']) for item in body['input']
                               if item['type'] == 'function_call_output'}
                    assert results['parallel_left']['exit_code'] == 0, results
                    assert results['parallel_right']['exit_code'] == 0, results
                    assert results['parallel_left']['stdout'] == 'left-overlapped'
                    assert results['parallel_right']['stdout'] == 'right-overlapped'
                    assert all(results[call['id']]['ok'] for call in responses[index-1]['calls'])
                contexts = [item for item in body['input'] if item.get('role') == 'developer']
                assert contexts, 'missing initial runtime context'
                context = json.loads(contexts[-1]['content'][0]['text'])
                assert context['type'] == 'runtime_context'
                last = next(item for item in reversed(body['input']) if item.get('role') != 'developer')
                if last['type'] == 'function_call_output':
                    message = 'tool: ' + last['output']
                    assert json.loads(last['output'])['ok'] is True
                else:
                    message = last['role'] + ': ' + ''.join(part.get('text', '') for part in last['content'])
                assert message.startswith(response['prefix']), (message, response['prefix'])
                self.send_response(200)
                self.send_header('Content-Type', 'text/event-stream')
                self.end_headers()
                def event(value):
                    self.wfile.write(('data: ' + json.dumps(value) + '\n\n').encode())
                    self.wfile.flush()
                if response.get('text'):
                    event({'type': 'response.output_text.delta', 'delta': response['text']})
                for output_index, call in enumerate(response.get('calls', [])):
                    arguments = json.dumps(call['arguments'])
                    item = {'type': 'function_call', 'id': 'item_' + call['id'], 'call_id': call['id'],
                            'name': call['name'], 'arguments': ''}
                    event({'type': 'response.output_item.added', 'output_index': output_index, 'item': item})
                    for fragment in (arguments[:3], arguments[3:]):
                        event({'type': 'response.function_call_arguments.delta', 'output_index': output_index,
                               'item_id': item['id'], 'delta': fragment})
                    event({'type': 'response.function_call_arguments.done', 'output_index': output_index,
                           'item_id': item['id'], 'arguments': arguments})
                    item['arguments'] = arguments
                    event({'type': 'response.output_item.done', 'output_index': output_index, 'item': item})
                if response.get('text'):
                    event({'type': 'response.output_item.done', 'output_index': len(response.get('calls', [])),
                           'item': {'type': 'message', 'id': 'msg_' + str(index), 'role': 'assistant',
                                    'status': 'completed', 'phase': 'final_answer',
                                    'content': [{'type': 'output_text', 'text': response['text'], 'annotations': []}]}})
                event({'type': 'response.completed', 'response': {'id': 'mock_' + str(index),
                    'service_tier': body['service_tier'], 'usage': {'input_tokens': 100, 'output_tokens': 10}}})
            except Exception:
                server_errors.append(traceback.format_exc())
                (root / 'mock-failure.txt').write_text('\n'.join(server_errors))
                self.send_error(500)

    server = ThreadingHTTPServer(('127.0.0.1', 0), MockOpenAI)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    endpoint = 'http://127.0.0.1:' + str(server.server_port)
    auth = root / 'mock-auth.json'
    auth.write_text(json.dumps({'auth_mode': 'chatgpt', 'tokens': {
        'access_token': 'mock-token', 'account_id': 'mock-account'}}))
    auth.chmod(0o600)
    master, slave = pty.openpty()
    process = subprocess.Popen([
        str(pathlib.Path(args.binary).resolve()), '--plain', '--data-dir', str(root / 'data'),
        '--workdir', str(workspace), '--openai-base-url', endpoint, '--import-codex-auth', str(auth),
        '--model', 'mock-model', '--variant', 'low', '--auto-name=false',
    ], stdin=slave, stdout=slave, stderr=slave, start_new_session=True)
    os.close(slave)
    output = bytearray()
    cursor = 0

    def save():
        (root / 'transcript.log').write_bytes(output)

    def expect(needle, timeout=8):
        nonlocal cursor
        target = needle.encode()
        deadline = time.monotonic() + timeout
        while True:
            pos = output.find(target, cursor)
            if pos >= 0:
                cursor = pos + len(target)
                save()
                return
            if time.monotonic() >= deadline:
                save()
                raise AssertionError(f'timed out waiting for {needle!r}')
            if select.select([master], [], [], 0.1)[0]:
                try:
                    data = os.read(master, 65536)
                except OSError:
                    data = b''
                if not data:
                    save()
                    raise AssertionError(f'PTY exited waiting for {needle!r}; exit={process.poll()}')
                output.extend(data)

    def send(text):
        os.write(master, text.encode() + b'\n')

    try:
        send('Create a result file')
        expect('Retrying · attempt 2')
        expect('Created scripted result.')
        expect('Turn complete')
        assert (workspace / 'result.txt').read_text() == 'verified\n'
        send('/undo')
        expect('undo completed')
        assert not (workspace / 'result.txt').exists()
        send('/redo')
        expect('redo completed')
        assert (workspace / 'result.txt').read_text() == 'verified\n'
        db = sqlite3.connect(root / 'data/history.sqlite')
        session = db.execute('SELECT id FROM sessions WHERE read_only=0').fetchone()[0]
        retry_id, retry_json, visible = db.execute("SELECT id,content_json,model_visible FROM entries WHERE json_extract(content_json,'$.type')='model_retry'").fetchone()
        retry = json.loads(retry_json)
        assert not visible and retry['retry'] == {'attempt': 2, 'max_attempts': 0, 'delay_ms': 0, 'reason': 'HTTP 503'}
        assert db.execute('SELECT purpose FROM model_requests WHERE id=?', (retry['request_id'],)).fetchone()[0] == 'coding'
        assert failed_request == requests[0], 'retry changed the request'
        send(f'/inspect {retry_id}')
        expect('model_retry')
        prompt = db.execute("SELECT id FROM entries WHERE json_extract(content_json,'$.type')='system_prompt' LIMIT 1").fetchone()[0]
        send(f'/inspect {prompt}')
        expect('You are TTC')
        export = root / 'export.md'
        send(f'/export {export}')
        expect('Exported')
        assert 'You are TTC' not in export.read_text()
        assert 'You are TTC' in pathlib.Path(str(export) + '.jsonl').read_text()
        assert pathlib.Path(str(export) + '.assets').is_dir()
        send('Parallel independent shell and file tools')
        expect('Running independent tools.')
        deadline = time.monotonic() + 5
        while not (workspace / 'left.ready').exists() or not (workspace / 'right.ready').exists():
            assert time.monotonic() < deadline, 'shell batch did not start'
            time.sleep(0.01)
        send('/model mock-model/fast low')
        expect('Model selected for next tool boundary')
        (workspace / 'batch.release').touch()
        expect('Model switched')
        expect('Parallel batch verified.')
        expect('Turn complete')
        assert (workspace / 'ordered.txt').read_text() == 'second\n'
        send('/undo')
        expect('undo completed')
        assert not (workspace / 'ordered.txt').exists()
        send('/redo')
        expect('redo completed')
        assert (workspace / 'ordered.txt').read_text() == 'second\n'
        send('Background a short command')
        expect('Background job launched.')
        expect('Turn complete')
        expect('Background completion observed.')
        expect('Turn complete')
        send('/jobs')
        expect('background-complete')  # Label includes the exact command; durable output checked below.
        job_records = db.execute("SELECT content_json FROM entries WHERE json_extract(content_json,'$.type')='job_completion'").fetchall()
        assert any('background-complete' in json.loads(row[0])['job']['stdout'] for row in job_records)
        assert all(json.loads(row[0])['markdown']['revision'] == 1 for row in job_records)
        switches = db.execute("SELECT content_json FROM entries WHERE json_extract(content_json,'$.type')='model_switch'").fetchall()
        assert len(switches) == 1 and json.loads(switches[0][0])['selection']['model']['service_tier'] == 'priority'
        send('Long job to cancel on switch')
        expect('Long job started.')
        expect('Turn complete')
        send('/new')
        expect('New session')
        send('/jobs')
        expect('[]')
        send(f'/load {session}')
        expect('Loaded')
        send('/jobs')
        expect('[]')
        send('/quit')
        assert process.wait(timeout=5) == 0
        assert (workspace / 'result.txt').read_text() == 'verified\n'
        assert b'Error:' not in output, output.decode(errors='replace')
        assert not server_errors, server_errors
        assert len(requests) == len(responses), (len(requests), len(responses))
        prompt_path = db.execute("SELECT json_extract(content_json,'$.path') FROM entries WHERE id=?", (prompt,)).fetchone()[0]
        assert pathlib.Path(prompt_path).read_text() == requests[0]['instructions']
        (root / 'mock-requests.json').write_text(json.dumps(requests, indent=2))
        db.close()
        save()
        print(f'PASS: HTTP retry/UI notice/inspection, parallel tool batch/ordered writes, boundary Fast switch, live tool cards, tool cycle, system inspection, undo/redo, export, background notification, switch cancellation, history reload; artifacts: {root}')
    except BaseException:
        save()
        (root / 'failure.txt').write_text(traceback.format_exc())
        print(f'Failure context saved: {root}')
        raise
    finally:
        if process.poll() is None:
            os.killpg(process.pid, signal.SIGKILL)
            process.wait()
        os.close(master)
        server.shutdown()
        server.server_close()
        thread.join()


if __name__ == '__main__':
    main()

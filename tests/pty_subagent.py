#!/usr/bin/env python3
"""Disposable subagent in a PTY: mock HTTP background, or socket-free foreground."""
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
    parser.add_argument('--offline', action='store_true',
                        help='Use the scripted provider: foreground disposal and bounded answer, no sockets')
    args = parser.parse_args()
    root = Path(tempfile.mkdtemp(prefix='pty-subagent-', dir=private_scratch()))
    print(f'Artifacts: {root}', flush=True)
    project = root / 'project'
    project.mkdir()
    (project / 'evidence.txt').write_text('Measured value: 42 µm.\n')
    prompt = 'Audit the fixture. PARENT-ONLY-MARKER'
    answer = '## Audit complete\n\nThe evidence reports **42 µm**. Unicode: α → β.'
    if args.offline:
        answer += '\n测量 **42 µm**. α → β.\n' * 800
    requests, errors, counts = [], [], {}
    lock = threading.Lock()
    notice_requests = []

    def texts(body, role=None):
        return [''.join(part.get('text', '') for part in item.get('content', []))
                for item in body['input'] if item.get('role') and
                (role is None or item['role'] == role)]

    class Mock(BaseHTTPRequestHandler):
        def log_message(self, *unused):
            pass

        def do_GET(self):
            if not self.path.startswith('/models?'):
                self.send_error(404)
                return
            body = {'models': [{'slug': 'mock-audit', 'display_name': 'Mock Audit',
                'visibility': 'list', 'context_window': 100000,
                'default_reasoning_level': 'high', 'supported_reasoning_levels':
                [{'effort': 'high'}, {'effort': 'low'}]}]}
            self.send_response(200)
            self.send_header('Content-Type', 'application/json')
            self.end_headers()
            self.wfile.write(json.dumps(body).encode())

        def do_POST(self):
            try:
                assert self.path == '/responses'
                body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
                context = json.loads(texts(body, 'developer')[-1])
                actor = context['actor']
                child = actor != 'main'
                with lock:
                    step = counts.get(actor, 0)
                    counts[actor] = step + 1
                    requests.append({'actor': actor, 'body': body,
                                     'session_id': self.headers.get('session-id')})
                    (root / 'requests.json').write_text(json.dumps(requests, indent=2))
                assert self.headers['Authorization'] == 'Bearer mock-token'
                assert self.headers['ChatGPT-Account-ID'] == 'mock-account'
                assert body['store'] is False and body['stream'] is True
                assert body['parallel_tool_calls'] is True
                assert body['model'] == 'mock-audit'
                assert body['reasoning']['effort'] == ('low' if child else 'high')
                assert body['prompt_cache_key'] == self.headers['session-id']
                results = {item['call_id']: json.loads(item['output'])
                           for item in body['input'] if item['type'] == 'function_call_output'}
                calls, text = [], ''
                if child:
                    assert step < 2, 'unexpected child request'
                    assert all('PARENT-ONLY-MARKER' not in text for text in texts(body))
                    assert 'subagent' not in [tool['name'] for tool in body['tools']]
                    if step == 0:
                        assert texts(body, 'user') == ['Audit evidence.txt and report the measurement.']
                        calls = [('read-evidence', 'read', {'path': 'evidence.txt'})]
                    else:
                        assert results['read-evidence']['ok'] is True
                        assert '42 µm' in results['read-evidence']['content']
                        text = answer
                elif step == 0:
                    assert texts(body, 'user')[0] == prompt
                    definition = next(tool for tool in body['tools'] if tool['name'] == 'subagent')
                    assert 'persistent' in definition['parameters']['required']
                    calls = [('audit-child', 'subagent', {
                        'prompt': 'Audit evidence.txt and report the measurement.',
                        'label': 'audit', 'persistent': False, 'background': True, 'variant': 'low'})]
                else:
                    assert step < 3, 'unexpected parent request'
                    launch = results['audit-child']
                    assert launch['ok'] is True and launch['persistent'] is False
                    assert not {'stdout', 'answer', 'result_entry_id', 'finish_event_seq'} & launch.keys()
                    notices = []
                    for message in texts(body, 'user'):
                        if message.startswith('{'):
                            event = json.loads(message)
                            if event.get('type') == 'child_turn_finished':
                                notices.append(event)
                    if notices:
                        assert len(notices) == 1 and notices[0]['answer'] == answer
                        assert not notices[0].get('answer_truncated', False)
                        assert notices[0]['result_entry_id'] > 0
                        assert notices[0]['persistent'] is False
                        with lock:
                            notice_requests.append(body['prompt_cache_key'])
                        text = 'Parent received the Unicode audit answer directly.'
                    else:
                        text = 'Background audit launched.'
                self.send_response(200)
                self.send_header('Content-Type', 'text/event-stream')
                self.end_headers()

                def emit(event):
                    self.wfile.write(('data: ' + json.dumps(event) + '\n\n').encode())
                    self.wfile.flush()

                for index, (call_id, name, arguments) in enumerate(calls):
                    encoded = json.dumps(arguments)
                    item = {'type': 'function_call', 'id': 'item_' + call_id,
                            'call_id': call_id, 'name': name, 'arguments': ''}
                    emit({'type': 'response.output_item.added', 'output_index': index, 'item': item})
                    for fragment in (encoded[:7], encoded[7:]):
                        emit({'type': 'response.function_call_arguments.delta',
                              'output_index': index, 'item_id': item['id'], 'delta': fragment})
                    emit({'type': 'response.function_call_arguments.done', 'output_index': index,
                          'item_id': item['id'], 'arguments': encoded})
                    item['arguments'] = encoded
                    emit({'type': 'response.output_item.done', 'output_index': index, 'item': item})
                if text:
                    emit({'type': 'response.output_text.delta', 'delta': text})
                    emit({'type': 'response.output_item.done', 'output_index': 0,
                          'item': {'type': 'message', 'id': f'msg_{actor}_{step}',
                                   'role': 'assistant', 'status': 'completed', 'phase': 'final_answer',
                                   'content': [{'type': 'output_text', 'text': text, 'annotations': []}]}})
                emit({'type': 'response.completed', 'response': {'id': f'resp_{actor}_{step}',
                      'usage': {'input_tokens': 100, 'output_tokens': 10,
                                'input_tokens_details': {'cached_tokens': 32},
                                'output_tokens_details': {'reasoning_tokens': 2}}}})
            except Exception:
                with lock:
                    errors.append(traceback.format_exc())
                    (root / 'mock-failure.txt').write_text('\n'.join(errors))
                self.send_error(500)

    server = worker = process = master = None
    output = bytearray()
    try:
        command = [str(Path(args.binary).resolve()), '--plain', '--workdir', str(project),
                   '--data-dir', str(root / 'data'), '--auto-name=false']
        if args.offline:
            script = root / 'script.json'
            script.write_text(json.dumps([
                {'prefix': 'user: ' + prompt, 'calls': [{'id': 'audit-child', 'name': 'subagent',
                    'arguments': {'prompt': 'Audit evidence.txt and report the measurement.',
                                  'label': 'audit', 'persistent': False, 'variant': 'none'}}]},
                {'prefix': 'user: Audit evidence.txt', 'text': answer},
                {'prefix': 'tool: ', 'text': 'Parent received the Unicode audit answer directly.'},
            ]))
            command += ['--offline-script', str(script)]
        else:
            server = ThreadingHTTPServer(('127.0.0.1', 0), Mock)
            worker = threading.Thread(target=server.serve_forever, daemon=True)
            worker.start()
            auth = root / 'mock-auth.json'
            auth.write_text(json.dumps({'auth_mode': 'chatgpt', 'tokens': {
                'access_token': 'mock-token', 'account_id': 'mock-account'}}))
            auth.chmod(0o600)
            command += ['--openai-base-url', f'http://127.0.0.1:{server.server_port}',
                        '--import-codex-auth', str(auth), '--model', 'mock-audit', '--variant', 'high']
        master, slave = pty.openpty()
        try:
            process = subprocess.Popen(command,
                stdin=slave, stdout=slave, stderr=slave, start_new_session=True)
        finally:
            os.close(slave)
        os.write(master, (prompt + '\n').encode())
        deadline = time.monotonic() + 15
        while True:
            final_at = output.find(b'Parent received the Unicode audit answer directly.')
            if final_at >= 0 and output.find(b'Turn complete', final_at) >= 0:
                break
            assert time.monotonic() < deadline, 'timed out waiting for parent completion'
            if select.select([master], [], [], 0.1)[0]:
                chunk = os.read(master, 65536)
                assert chunk, f'PTY exited: {process.poll()}'
                output.extend(chunk)
                (root / 'terminal.log').write_bytes(output)
        os.write(master, b'/quit\n')
        deadline = time.monotonic() + 5
        while process.poll() is None:
            assert time.monotonic() < deadline, 'shutdown timed out'
            if select.select([master], [], [], 0.1)[0]:
                try:
                    output.extend(os.read(master, 65536))
                except OSError:
                    break
        assert process.wait(timeout=3) == 0
        assert not errors, errors
        assert b'Error:' not in output
        if not args.offline:
            assert len(notice_requests) == 1, notice_requests
            assert len(counts) == 2 and counts['main'] in (2, 3), counts
            child_actor = next(actor for actor in counts if actor != 'main')
            assert counts[child_actor] == 2
            main_key = next(r['body']['prompt_cache_key'] for r in requests if r['actor'] == 'main')
            child_keys = {r['body']['prompt_cache_key'] for r in requests if r['actor'] == child_actor}
            assert len(child_keys) == 1 and main_key not in child_keys
        with sqlite3.connect(root / 'data/history.sqlite') as db:
            finishes = db.execute("SELECT id,content_json FROM entries WHERE json_extract(content_json,'$.type')='child_turn_finished'").fetchall()
            assert len(finishes) == 1
            event_id, finish_json = finishes[0]
            finish = json.loads(finish_json)
            assert finish['status'] == 'completed' and finish['persistent'] is False
            if args.offline:
                assert finish['answer_truncated'] is True
                assert answer.startswith(finish['answer']) and len(finish['answer'].encode()) <= 8192
                result = json.loads(db.execute("SELECT result_json FROM tool_calls WHERE name='subagent'").fetchone()[0])
                assert result['answer'] == finish['answer'] and result['answer_truncated'] is True
                assert result['persistent'] is False and 'stdout' not in result
            else:
                assert finish['answer'] == answer
            exact = json.loads(db.execute('SELECT content_json FROM entries WHERE id=?', (finish['result_entry_id'],)).fetchone()[0])
            assert exact['content'] == answer
            states = db.execute("SELECT content_json FROM entries WHERE json_extract(content_json,'$.type')='child_state' ORDER BY id").fetchall()
            assert json.loads(states[-1][0])['child']['state'] == 'closed'
            assert db.execute("SELECT count(*) FROM tool_calls WHERE name IN ('job_read','job_stop')").fetchone()[0] == 0
            assert db.execute("SELECT count(*) FROM model_requests,json_each(delivered_events_json) WHERE value=?", (event_id,)).fetchone()[0] == 1
            meters = db.execute('SELECT status,attempts_json FROM model_requests').fetchall()
            expected_requests = 3 if args.offline else len(requests)
            assert len(meters) == expected_requests
            usage = [json.loads(attempts)[-1]['usage'] for status, attempts in meters if status == 'completed']
            assert len(usage) == expected_requests
            if not args.offline:
                assert sum(u['input_tokens'] for u in usage) == 100 * expected_requests
                assert sum(u['cached_input_tokens'] for u in usage) == 32 * expected_requests
        coverage = ('offline foreground disposal, UTF-8 bounded answer, exact durable reply, three metered requests'
                    if args.offline else 'HTTP low-variant child, isolated read, background answer delivered once, metering')
        print(f'PASS: {coverage}, closed context; artifacts: {root}')
    except BaseException:
        (root / 'failure.txt').write_text(traceback.format_exc())
        raise
    finally:
        (root / 'terminal.log').write_bytes(output)
        if process is not None and process.poll() is None:
            try:
                os.killpg(process.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            process.wait()
        if master is not None:
            os.close(master)
        if server is not None:
            if worker is not None and worker.is_alive():
                server.shutdown()
                worker.join()
            server.server_close()
        with lock:
            (root / 'requests.json').write_text(json.dumps(requests, indent=2))


if __name__ == '__main__':
    main()

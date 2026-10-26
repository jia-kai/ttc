#!/usr/bin/env python3
"""Reproducible local OpenAI mock: all 20 tool types and a Markdown notebook.

Run normally for automated plain PTY validation, --tui for keyboard/menu testing,
or --interactive to inspect the full TUI yourself. No real credentials or tokens.
"""
import argparse
import errno
import fcntl
import json
import os
from pathlib import Path
import pty
import re
import select
import shlex
import shutil
import signal
import sqlite3
import struct
import subprocess
import termios
import tempfile
import textwrap
import threading
import time
import traceback
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


from scratch import private_scratch
from mock_naming import respond_to_naming
from pty_screen import Screen
from child_fixture import CHILD_ANSWER, CHILD_PREFIX, CHILD_TAIL, child_response_row, child_response_target, compact_rows, demo_completed, verify_child_answers

def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary', default='./ttc')
    parser.add_argument('--visual-hold', action='store_true', help='Pause with live job/timer until the visual driver releases visual.ready')
    parser.add_argument('--artifacts-file', type=Path, help='Write the private demo root for an automated visual driver')
    modes = parser.add_mutually_exclusive_group()
    modes.add_argument('--interactive', action='store_true')
    modes.add_argument('--tui', action='store_true')
    args = parser.parse_args()
    scratch = private_scratch()
    root = Path(tempfile.mkdtemp(prefix='demo-', dir=scratch))
    if args.artifacts_file:
        args.artifacts_file.write_text(json.dumps({'root': str(root)}))
    fixture = Path(__file__).resolve().parent / 'fixtures/research'
    workspace = root / 'project'
    shutil.copytree(fixture, workspace)
    lsp_fixture = workspace / '.lsp/server.fixture'
    lsp_fixture.parent.mkdir(mode=0o700)
    shutil.copyfile(Path(__file__).resolve().parent.parent / 'internal/lsp/testdata/server.py', lsp_fixture)
    if shutil.which("git"):
        subprocess.run(["git", "-C", str(workspace), "init", "--quiet", "-b", "demo"], check=True)
    markdown = (fixture / 'markdown.md').read_text()
    auth = root / 'mock-auth.json'
    auth.write_text(json.dumps({'auth_mode': 'chatgpt', 'tokens': {'access_token': 'mock-token', 'account_id': 'mock-account'}}))
    auth.chmod(0o600)
    requests, errors = [], []
    main_requests, child_requests = [], []

    call_serial = 0
    def call(name, arguments):
        nonlocal call_serial
        call_serial += 1
        return {'type': 'function_call', 'id': f'item_{call_serial}_{name}', 'call_id': f'demo_{call_serial}_{name}', 'name': name,
                'arguments': json.dumps(arguments)}

    class Mock(BaseHTTPRequestHandler):
        def log_message(self, *unused):
            pass

        def do_GET(self):
            if self.path.startswith('/models?'):
                assert 'client_version=0.159.0' in self.path
                models = []
                for model, name in (('demo-a', 'Demo Sol'), ('demo-b', 'Demo Luna')):
                    models.append({'slug': model, 'display_name': name, 'visibility': 'list',
                        'context_window': 100000, 'default_reasoning_level': 'low',
                        'supported_reasoning_levels': [{'effort': 'low', 'description': 'Quick'},
                                                      {'effort': 'high', 'description': 'Deeper'}]})
                models.append({'slug': 'internal-review', 'visibility': 'hide'})
                body, kind = json.dumps({'models': models}).encode(), 'application/json'
            elif self.path == '/notes':
                body, kind = b'<h1>Local evidence</h1><p>Three observations, mean 4.0.</p>', 'text/html'
            else:
                self.send_error(404)
                return
            self.send_response(200)
            self.send_header('Content-Type', kind)
            self.end_headers()
            self.wfile.write(body)

        def do_POST(self):
            try:
                if self.path == '/mcp':
                    body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
                    assert body['params']['name'] == 'web_search_exa'
                    assert body['params']['arguments']['query'] == 'local fixture observations'
                    result = {'jsonrpc': '2.0', 'id': body['id'], 'result': {'content': [{'type': 'text', 'text': 'Title: Local evidence\nURL: ' + endpoint + '/notes\nHighlights: Three observations, mean 4.0.'}]}}
                    self.send_response(200)
                    self.send_header('Content-Type', 'application/json')
                    self.end_headers()
                    self.wfile.write(json.dumps(result).encode())
                    return
                assert self.path == '/responses' 
                assert self.headers['Authorization'] == 'Bearer mock-token'
                assert self.headers['ChatGPT-Account-ID'] == 'mock-account'
                body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
                if respond_to_naming(self, body):
                    return
                requests.append(body)
                (root / 'requests.json').write_text(json.dumps(requests, indent=2))
                expected_model, effort = ('demo-b', 'high') if args.tui else ('demo-a', 'low')
                assert body['model'] == expected_model and body['reasoning']['effort'] == effort
                assert body['parallel_tool_calls'] is True
                assert body['stream'] and not body['store'] and 'max_output_tokens' not in body
                # Runtime context starts with a baseline followed by field updates.
                contexts = [item for item in body['input'] if item.get('role') == 'developer']
                assert contexts, 'missing initial runtime context'
                context = json.loads(contexts[0]['content'][0]['text'])
                assert context['type'] == 'runtime_context'
                for item in contexts[1:]:
                    payload = json.loads(item['content'][0]['text'])
                    assert payload['type'] in ('runtime_context', 'runtime_context_update')
                    if payload['type'] == 'runtime_context':
                        context = payload
                    else:
                        assert isinstance(payload['set'], dict)
                last = next(item for item in reversed(body['input']) if item.get('role') != 'developer')
                child = context['actor'] != 'main'
                # Terminal job notifications now always arrive. Acknowledge
                # event-only turns without advancing the scripted tool cycle.
                conversation = child_requests if child else main_requests
                previous_input = conversation[-1]['input'] if conversation else []
                new_results = any(item['type'] == 'function_call_output'
                                  for item in body['input'][len(previous_input):])
                runtime_notice = False
                if last.get('role') == 'user':
                    text = last['content'][0].get('text', '')
                    notice = json.loads(text) if text.startswith('{') else {}
                    runtime_notice = notice.get('message_type') == 'runtime_notification'
                    if runtime_notice and not new_results:
                        self.send_response(200)
                        self.send_header('Content-Type', 'text/event-stream')
                        self.end_headers()
                        event = {'type': 'response.completed', 'response': {'id': 'notice-ack',
                                 'usage': {'input_tokens': 10, 'output_tokens': 0}}}
                        self.wfile.write(('data: ' + json.dumps(event) + '\n\n').encode())
                        return
                if conversation:
                    assert body['instructions'] == conversation[0]['instructions']
                    previous_input = conversation[-1]['input']
                    assert body['input'][:len(previous_input)] == previous_input
                conversation.append(body)
                stage = len(conversation) - 1
                if stage == 0 and not child:
                    assert last['role'] == 'user' and last['content'][0]['text'].startswith('Run demo')
                elif stage > 0 and stage < 6 and (not child or stage == 1):
                    assert last['type'] == 'function_call_output' or (runtime_notice and new_results)
                results = {}
                for item in body['input']:
                    if item['type'] == 'function_call_output':
                        result = json.loads(item['output'])
                        assert result['ok'], result
                        results[item['call_id'].split('_', 2)[-1]] = result
                        if result.get('kind') == 'lsp':
                            results['lsp_job'] = result
                if child:
                    assert stage <= 2
                    if args.visual_hold and stage == 1:
                        deadline = time.monotonic() + 45
                        while not (workspace / 'child.ready').exists():
                            assert time.monotonic() < deadline, 'visual driver did not release child'
                            time.sleep(0.01)
                    calls = [call('read', {'path': 'observations.csv'})] if stage == 0 else []
                elif stage == 0:
                    calls = [call('read', {'path': 'README.md'}), call('glob', {'pattern': '**/*.py'}),
                             call('grep', {'pattern': 'mean', 'include': '*.py'}),
                             call('image_show', {'path': 'field.png', 'request_click': args.interactive and context['image_click']}), call('skill', {'name': 'file-based-plan'})]
                elif stage == 1:
                    calls = [call('write', {'path': 'report.txt', 'content': 'draft\n'}),
                             call('edit', {'path': 'report.txt', 'old_text': 'draft', 'new_text': 'verified'}),
                             call('patch', {'patch_text': '*** Begin Patch\n*** Add File: note.txt\n+Mean is 4.0.\n*** End Patch'}),
                             call('shell', {'command': "python3 analysis.py && printf ''"}),
                             call('subagent', {'persistent': True, 'prompt': 'Check fixture observations independently.', 'label': 'Fixture check'}),
                             call('web_fetch', {'url': endpoint + '/notes'}),
                             call('web_search', {'query': 'local fixture observations'}),
                             call('wakeup_schedule', {'name': 'demo', 'message': 'Check observations', 'delay_seconds': 600})]
                elif stage == 2:
                    calls = [call('wakeup_list', {}),
                             call('subagent', {'persistent': True, 'child_id': results['subagent']['child_id'], 'prompt': 'Verify the final fixture report.'}),
                             call('shell', {'command': 'python3 .lsp/server.fixture', 'background': True, 'protocol': 'lsp'}),
                             call('shell', {'command': "printf 'background evidence\\n'; touch background.ready; sleep 30",
                                           'background': True})]
                elif stage == 3:
                    if args.visual_hold:
                        deadline = time.monotonic() + 45
                        while not (workspace / 'visual.ready').exists():
                            assert time.monotonic() < deadline, 'visual driver did not release demo'
                            time.sleep(0.01)
                    deadline = time.monotonic() + 5
                    while not (workspace / 'background.ready').exists():
                        assert time.monotonic() < deadline, 'background fixture did not become ready'
                        time.sleep(0.01)
                    job = results['shell']['job_id']
                    calls = [call('lsp_query', {'job_id': results['lsp_job']['job_id'], 'operation': 'hover', 'path': 'analysis.py', 'line': 1, 'column': 1}),
                             call('job_stop', {'child_id': results['subagent']['child_id']}),
                             call('job_list', {'state': 'all'}), call('job_read', {'job_id': job, 'stream': 'stdout', 'grep': 'evidence'}),
                             call('job_stop', {'job_id': job}), call('wakeup_cancel', {'name': 'demo'})]
                elif stage == 4:
                    calls = [call('question', {'questions': [{'id': 'continue', 'prompt': 'Finish the Markdown report?',
                        'recommended_option_id': 'yes',
                        'options': [{'id': 'yes', 'label': 'Yes', 'description': 'The checks have passed.'}, {'id': 'no', 'label': 'No'}]},
                        {'id': 'notes', 'prompt': 'Any notes for the report?',
                         'options': [{'id': 'brief', 'label': 'Brief'}, {'id': 'detailed', 'label': 'Detailed'}]},
                        {'id': 'checks', 'prompt': 'Which evidence should be retained?',
                         'options': [{'id': 'data', 'label': 'Data'}, {'id': 'render', 'label': 'Rendering'}]}]})]
                elif stage == 5:
                    calls = []
                elif stage == 6 and not child:
                    click = json.loads(last['content'][0]['text'])
                    assert last['role'] == 'user' and click['type'] in ('image_click', 'image_click_cancelled')
                    if click['type'] == 'image_click':
                        assert click['width'] == 320 and click['height'] == 200 and click['precision'] == 'cell'
                        assert 0 <= click['x'] < 320 and 0 <= click['y'] < 200
                    calls = []
                else:
                    raise AssertionError('demo script exhausted; start a new demo process')
                self.send_response(200)
                self.send_header('Content-Type', 'text/event-stream')
                self.end_headers()
                events = []
                # Announce all calls first, then interleave split argument strings.
                for index, item in enumerate(calls):
                    events.append({'type': 'response.output_item.added', 'output_index': index,
                                   'item': dict(item, arguments='', status='in_progress')})
                for half in range(2):
                    for index, item in enumerate(calls):
                        arguments = item['arguments']
                        split = len(arguments) // 2
                        events.append({'type': 'response.function_call_arguments.delta',
                                       'output_index': index, 'item_id': item['id'],
                                       'delta': arguments[:split] if half == 0 else arguments[split:]})
                for index, item in enumerate(calls):
                    events.extend([
                        {'type': 'response.function_call_arguments.done', 'output_index': index,
                         'item_id': item['id'], 'arguments': item['arguments']},
                        {'type': 'response.output_item.done', 'output_index': index,
                         'item': dict(item, status='completed')}])
                if child and stage in (1, 2):
                    # Split at line boundaries to exercise growing multiline child previews.
                    for chunk in CHILD_ANSWER.splitlines(keepends=True):
                        events.append({'type': 'response.output_text.delta', 'delta': chunk})
                elif not child and stage == 5:
                    events.append({'type': 'response.output_text.delta', 'delta': markdown})
                elif not child and stage == 6:
                    events.append({'type': 'response.output_text.delta', 'delta': 'Image point confirmed.' if click['type'] == 'image_click' else 'Image click cancelled.'})
                text = ''.join(e['delta'] for e in events if e['type'] == 'response.output_text.delta')
                if text:
                    events.append({'type': 'response.output_item.done', 'output_index': len(calls),
                                   'item': {'type': 'message', 'id': f'msg_{stage}', 'role': 'assistant',
                                            'status': 'completed', 'phase': 'final_answer',
                                            'content': [{'type': 'output_text', 'text': text, 'annotations': []}]}})
                events.append({'type': 'response.completed', 'response': {'id': f'demo_{stage}',
                    'usage': {'input_tokens': 100, 'input_tokens_details': {'cached_tokens': 80}, 'output_tokens': 10, 'output_tokens_details': {'reasoning_tokens': 3}}}})
                for event in events:
                    self.wfile.write(('data: ' + json.dumps(event) + '\n\n').encode())
                    self.wfile.flush()
                    if args.visual_hold and not child and stage == 0 and event['type'] == 'response.output_item.added':
                        deadline = time.monotonic() + 45
                        while not (workspace / 'stream.ready').exists():
                            if time.monotonic() > deadline:
                                raise TimeoutError('visual driver did not release tool argument stream')
                            time.sleep(0.01)
            except Exception:
                errors.append(traceback.format_exc())
                (root / 'server-failure.txt').write_text('\n'.join(errors))
                self.send_error(500)

    server = ThreadingHTTPServer(('127.0.0.1', 0), Mock)
    endpoint = f'http://127.0.0.1:{server.server_port}'
    os.environ['TTC_EXA_URL'] = endpoint + '/mcp'
    os.environ.pop('EXA_API_KEY', None)
    binary = str(Path(args.binary).resolve())
    command = [binary, '--data-dir', str(root / 'data'), '--workdir', str(workspace),
               '--openai-base-url', endpoint, '--import-codex-auth', str(auth)]
    print(f'Demo project and artifacts: {root}', flush=True)
    if args.interactive:
        print('Type "Run demo". The question dialog has three tabs and a final Submit tab.\n'
              'Enter selects Yes and advances. Write "Looks good." under Other on Notes, select Data, then Submit.\n'
              'Use Ctrl+U/D to scroll, Ctrl+X F for fullscreen, Ctrl+X S for sidebar, and Tab/click for inspection.\n'
              'The child emits long evidence: its assistant and returned-answer rows stay compact; click either for the full tail.\n'
              'In Kitty, click the field thumbnail, pick a point and press OK. Keep Demo Sol / low; /quit exits. Kitty renders images and math; Ctrl+X F toggles fullscreen.', flush=True)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            subprocess.run(command, check=True)
        finally:
            server.shutdown()
            server.server_close()
            thread.join()
        if errors:
            raise AssertionError(errors)
        return

    if not args.tui:
        command.append('--plain')
    else:
        editor = root / 'question-editor.py'
        editor.write_text('''import sys
from pathlib import Path
path = Path(sys.argv[1])
text = path.read_text()
assert text == 'Looks bad.', repr(text)
(Path(__file__).parent / 'editor-original.txt').write_text(text)
path.write_text('Looks good.')
''')
    # Fork before starting server threads; forkpty supplies a proper controlling terminal.
    pid, master = pty.fork()
    if pid == 0:
        os.environ['TERM'] = 'xterm-256color'
        if args.tui:
            os.environ['VISUAL'] = 'python3 ' + shlex.quote(str(editor))
            os.environ['EDITOR'] = 'this-editor-must-not-run'
        os.execv(binary, command)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    fcntl.ioctl(master, termios.TIOCSWINSZ, struct.pack('HHHH', 42, 110, 0, 0))
    output, cursor, waited = bytearray(), 0, False
    screen = Screen(42, 110)

    def receive():
        data = os.read(master, 65536)
        output.extend(data)
        screen.feed(data)
        return data

    def save():
        (root / 'transcript.log').write_bytes(output)
        (root / 'screen.txt').write_text(screen.text)

    def expect(needle, timeout=10):
        nonlocal cursor
        deadline = time.monotonic() + timeout
        while True:
            pos = output.find(needle.encode(), cursor)
            if pos >= 0:
                cursor = pos + len(needle.encode())
                save()
                return
            if time.monotonic() > deadline:
                save()
                raise AssertionError(f'timed out waiting for {needle!r}')
            if select.select([master], [], [], 0.1)[0]:
                data = receive()
                if not data:
                    raise AssertionError('demo process exited early')

    def screen_expect(needle, present=True, timeout=10):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if (needle in screen.text) == present:
                save()
                return screen.text
            if select.select([master], [], [], 0.05)[0]:
                receive()
        save()
        raise AssertionError(f'screen expected {needle!r}, present={present}')

    def settled_screen():
        # Drain redraws before asserting absence; raw byte searches cannot
        # reconstruct tcell's cursor-addressed one-line previews.
        deadline = time.monotonic() + 0.2
        while time.monotonic() < deadline:
            if select.select([master], [], [], 0.02)[0]:
                receive()
        return screen.text

    def verify_child_inspectors():
        def reveal_tail():
            for _ in range(240):
                before = settled_screen()
                # Raw JSON strings can wrap the marker across terminal rows.
                body = ''.join(re.split('[│█]', line)[1] for line in before.splitlines() if '│' in line)
                if CHILD_TAIL in before or ''.join(CHILD_TAIL.split()) in ''.join(body.split()):
                    return
                os.write(master, b'\x1b[6~')
            raise AssertionError('child tail not reachable in grouped inspection: ' + screen.text)

        # The final notebook is long. Scroll up until both compact rows are in
        # the viewport, then test their actual mouse hit targets.
        for _ in range(35):
            text = settled_screen()
            if CHILD_PREFIX in text and '[Fixture check] assistant ' in text:
                try:
                    rows = compact_rows(text)
                    break
                except AssertionError:
                    # A page boundary can expose only one of the two row types.
                    if 'CHILD-HIDDEN-BODY' in text or CHILD_TAIL in text:
                        raise
            os.write(master, b'\x15')
        else:
            raise AssertionError('compact child and returned-answer rows not visible: ' + screen.text)
        (root / 'child-compact.txt').write_text(text)
        response, completion = rows
        os.write(master, f'\x1b[<0;8;{response}M\x1b[<0;8;{response}m'.encode())
        assert '┌' not in settled_screen(), 'direct assistant row opened payload inspection'
        compact_rows(screen.text)
        (root / 'child-direct-no-inspect.txt').write_text(screen.text)
        os.write(master, f'\x1b[<0;8;{completion}M\x1b[<0;8;{completion}m'.encode())
        screen_expect('Execution details')
        screen_expect(CHILD_PREFIX)
        os.write(master, b'\x1b[F')
        screen_expect(CHILD_TAIL)
        (root / 'child-completion-inspector.txt').write_text(screen.text)
        os.write(master, b'd')
        screen_expect('Parameters:')
        (root / 'child-completion-details.txt').write_text(screen.text)
        os.write(master, b'\x1b')
        screen_expect('┌', present=False)
        with sqlite3.connect(root / 'data/history.sqlite') as db:
            entry, title = child_response_target(db)
        os.write(master, b'\x1b')  # Follow tail before searching the native response.
        for _ in range(80):
            text = settled_screen()
            try:
                row = child_response_row(text, title)
                break
            except AssertionError:
                os.write(master, b'\x15')
        else:
            raise AssertionError('native child response not visible: ' + screen.text)
        (root / 'child-native-response-row.txt').write_text(text)
        os.write(master, f'\x1b[<0;8;{row}M\x1b[<0;8;{row}m'.encode())
        screen_expect('R raw')
        reveal_tail()
        (root / 'child-native-response-tail.txt').write_text(screen.text)
        os.write(master, b'r')
        screen_expect('Exact raw JSON')
        os.write(master, b'n')  # Select the completed response, not the outgoing request.
        reveal_tail()
        (root / 'child-native-response-raw-tail.txt').write_text(screen.text)
        os.write(master, b'\x1b')
        screen_expect('┌', present=False)
        # Explicit keyboard inspection of the boundary uses the same readable view.
        send(f'/inspect {entry}')
        screen_expect('┌')
        reveal_tail()
        os.write(master, b'\x1b')
        screen_expect('┌', present=False)

    def send(text):
        os.write(master, text.encode() + (b'\r' if args.tui else b'\n'))

    try:
        if args.tui:
            expect('Ctrl+X M')
            os.write(master, b'\x18m')
            expect('Model family')
            os.write(master, b'\x1b[B\r')
            expect('Reasoning variant')
            os.write(master, b'\x1b[B\r')
            expect('demo-b')
        send('Run demo')
        if args.tui:
            expect('Recommended')
            os.write(master, b'\r')  # Select Yes and advance to Notes.
            expect('Any notes')
            # Shared Ctrl+W editing and Ctrl+X E must operate on this answer.
            os.write(master, b'\x1b[B\x1b[B\rLooks obsolete\x17bad.\x18e')
            expect('Text: Looks good.')
            assert (root / 'editor-original.txt').read_text() == 'Looks bad.'
            os.write(master, b'\r')
            expect('Which evidence')
            os.write(master, b'\r')  # Select Data and advance to Submit.
            # The renderer may cursor-address unchanged spaces between words;
            # this single-word instruction is unique to the Submit tab.
            expect('submits')
            os.write(master, b'\r')
        else:
            expect('Waiting for answer')
            deadline = time.monotonic() + 5
            while not re.search(rb'form_[A-Za-z0-9_-]{16}', output) and time.monotonic() < deadline:
                if select.select([master], [], [], 0.1)[0]:
                    output.extend(os.read(master, 65536))
            form = re.search(rb'form_[A-Za-z0-9_-]{16}', output)
            assert form, 'question form ID was not presented'
            answers = [{'id': 'continue', 'values': ['yes'], 'source': 'option'},
                       {'id': 'notes', 'values': ['Looks good.'], 'source': 'custom'},
                       {'id': 'checks', 'values': ['data'], 'source': 'option'}]
            send('/answer ' + form[0].decode() + ' ' + json.dumps(answers))
        deadline = time.monotonic() + 15
        while not demo_completed(root / 'data/history.sqlite', markdown):
            assert time.monotonic() < deadline, 'main turn did not durably complete'
            if select.select([master], [], [], 0.05)[0]:
                receive()
        if args.tui:
            screen_expect('20 tool types')
            screen_expect('Turn complete')
            verify_child_inspectors()
            os.write(master, b'\x04')  # Empty-composer Ctrl+D must not exit.
            send('/model')
            expect('Model family')
            os.write(master, b'\x03')  # Ctrl+C exits even while the menu is open.
        else:
            expect('20 tool types')
            expect('Turn complete')
            compact_rows(output.decode('utf-8', 'replace'))
            send('/quit')
        deadline = time.monotonic() + 5
        while time.monotonic() < deadline:
            child, status = os.waitpid(pid, os.WNOHANG)
            if child:
                waited = True
                assert os.waitstatus_to_exitcode(status) == 0, status
                break
            # Drain redraws so PTY output cannot block the frontend before exit.
            if select.select([master], [], [], 0.05)[0]:
                try:
                    receive()
                except OSError as exc:
                    if exc.errno != errno.EIO:  # A closed Linux PTY reports EIO.
                        raise
        assert waited, 'demo process did not stop'
        assert not errors, errors
        assert len(main_requests) == 6 and len(child_requests) == 3, (len(main_requests), len(child_requests))
        db = sqlite3.connect(root / 'data/history.sqlite')
        records = [json.loads(row[0]) for row in db.execute('SELECT record_json FROM tool_records')]
        names = {record['name'] for record in records}
        expected = {'read', 'glob', 'grep', 'write', 'edit', 'patch', 'shell', 'job_list', 'job_read',
                    'job_stop', 'web_fetch', 'web_search', 'skill', 'image_show', 'question', 'wakeup_schedule',
                    'wakeup_list', 'wakeup_cancel', 'subagent', 'lsp_query'}
        assert names == expected, (names, expected)
        results = {}
        for record in records:
            result = record['result']
            assert result['ok'], record
            results.setdefault(record['name'], []).append(result)
        assert (results['image_show'][0]['width'], results['image_show'][0]['height']) == (320, 200)
        assert results['shell'][0]['stdout'] == 'Mean: 4.0\n'
        assert results['job_read'][0]['output'] == 'background evidence\n'
        grep_result = results['job_read'][0]
        assert grep_result['kind'] == 'grep'
        assert grep_result['job_id'] != grep_result['grep_origin']['source_job_id']
        assert grep_result['grep_origin']['match_count'] == 1
        assert any(result.get('status') == 'cancelled' for result in results['job_stop'])
        assert any(result.get('child_state') == 'closed'
                   and result.get('assignment_status') == 'completed'
                   and 'state' not in result and 'status' not in result
                   for result in results['job_stop'])
        assert results['lsp_query'][0]['kind'] == 'hover'
        assert '**fixture**' in results['lsp_query'][0]['markdown']
        assert results['subagent'][0]['child_id'] == results['subagent'][1]['child_id']
        assert results['subagent'][0]['child_turn_id'] != results['subagent'][1]['child_turn_id']
        assert results['wakeup_list'][0]['wakeups'][0]['status'] == 'scheduled'
        assert results['wakeup_cancel'][0]['status'] == 'cancelled'
        assert results['glob'][0]['paths'] == ['analysis.py', 'make_image.py']
        assert all(match['path'] == 'analysis.py' for match in results['grep'][0]['matches'])
        assert 'mean 4.0' in results['web_fetch'][0]['content']
        assert 'Highlights:' in results['web_search'][0]['content']
        verify_child_answers(db, results['subagent'])
        main_answers = [json.loads(item['output'])
                        for request in main_requests for item in request['input']
                        if item.get('type') == 'function_call_output'
                        and item['call_id'].endswith('_subagent')]
        verify_child_answers(db, main_answers)
        assert results['question'][0]['answers'][0]['values'] == ['yes']
        assert results['question'][0]['answers'][1]['values'] == ['Looks good.']
        assert results['question'][0]['answers'][1]['source'] == 'custom'
        assert results['question'][0]['answers'][2]['values'] == ['data']
        child_asset = (Path(__file__).resolve().parent.parent / 'prompt/system.yaml').read_text()
        # Child is a folded prose block: join wrapped lines within paragraphs,
        # preserve paragraph breaks and retain the final newline.
        child_block = re.search(r'^Child: >\n((?:  [^\n]*\n|\n)+)', child_asset, re.MULTILINE)
        assert child_block is not None, 'missing Child folded block'
        paragraphs = textwrap.dedent(child_block.group(1)).rstrip('\n').split('\n\n')
        child_instructions = '\n'.join(' '.join(p.splitlines()) for p in paragraphs) + '\n'
        assert child_requests[0]['instructions'].endswith(child_instructions)
        assert len(child_requests[0]['input']) == 2
        assert child_requests[0]['input'][0]['content'][0]['text'] == 'Check fixture observations independently.'
        child_visible = db.execute("SELECT count(*) FROM entries WHERE actor_id != 'main' AND model_visible=1").fetchone()[0]
        assert child_visible == 0
        assert (workspace / 'report.txt').read_text() == 'verified\n'
        assert (workspace / 'note.txt').read_text() == 'Mean is 4.0.\n'
        assistant = json.loads(db.execute("SELECT content_json FROM entries WHERE kind='message' AND role='assistant' ORDER BY id DESC LIMIT 1").fetchone()[0])
        assert assistant['content'] == markdown
        db.close()
        (root / 'tool-coverage.json').write_text(json.dumps(sorted(names), indent=2))
        save()
        print(f'PASS: {len(names)} selected tool types, reproducible project, exact Markdown report, ' +
              ('TUI model menu' if args.tui else 'plain PTY') + f'; artifacts: {root}')
    except BaseException:
        save()
        (root / 'failure.txt').write_text(traceback.format_exc())
        print(f'Failure context saved: {root}')
        raise
    finally:
        if not waited:
            os.killpg(pid, signal.SIGKILL)
            os.waitpid(pid, 0)
        os.close(master)
        server.shutdown()
        server.server_close()
        thread.join()


if __name__ == '__main__':
    main()

#!/usr/bin/env python3
"""Capture the real Kitty TUI under Xvfb against TTC's local mock server.

Requires kitty (with remote-control screenshot), Xvfb, and a software OpenGL
implementation. Tmux return comparisons also require Pillow. Saves screenshots,
terminal text, and complete failure logs.
"""
from pathlib import Path
import argparse
import json
import re
import shutil
import sqlite3
import subprocess
import tempfile
import time
import traceback
import unicodedata


from scratch import private_scratch
from kitty_terminal import KittyTerminal
from child_fixture import CHILD_PREFIX, CHILD_TAIL, child_response_row, child_response_target, compact_rows, demo_completed, verify_child_answers
from kitty_composer import check_composer, configure_editor
from kitty_question import check_question_cursor

def get_version() -> str:
    """an ad-hoc parser of go source to get the version string"""
    marker = 'const Version = '
    with open(Path(__file__).parent.parent / 'internal' / 'version' /
              'version.go') as fin:
        for line in fin:
            if line.startswith(marker):
                v = line[len(marker):].strip()
                assert v[0] == v[-1] == '"', f"bad {v=!r}"
                return v[1:-1]
    raise RuntimeError("could not find TTC version string")

def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary', default='./ttc')
    parser.add_argument('--tmux', action='store_true', help='Test auto-detection through a private tmux with Kitty identity cleared')
    parser.add_argument('--offline', action='store_true', help='Use a short scripted palette/image/math fixture without a TCP server')
    parser.add_argument('--composer-only', action='store_true', help='Run only offline multiline composer checks and screenshots')
    parser.add_argument('--question-only', action='store_true', help='Run only offline question native-cursor/edit/restore checks')
    args = parser.parse_args()
    if args.composer_only or args.question_only:
        args.offline = True
    scratch = private_scratch()
    root = Path(tempfile.mkdtemp(prefix='kitty-visual-', dir=scratch))
    kitty = KittyTerminal(root, tmux=args.tmux)
    env = kitty.env
    configure_editor(env, root)
    tmux_sock = kitty.tmux_sock
    remote = kitty.remote
    capture = kitty.capture
    last_text = ''
    try:
        kitty.start()
        binary = str(Path(args.binary).resolve())
        if args.offline:
            fixture = Path(__file__).with_name('fixtures') / 'research'
            project = root / 'project'
            shutil.copytree(fixture, project)
            script = root / 'script.json'
            script.write_text(json.dumps([
                {'prefix': 'user: Run demo', 'calls': [
                    {'id': 'image', 'name': 'image_show', 'arguments': {'path': 'field.png'}},
                    {'id': 'shell', 'name': 'shell', 'arguments': {'command': "printf 'Palette fixture verified\\n'"}},
                ]}, {'text': (fixture / 'markdown.md').read_text().replace(
                    'The demo is complete. All 20 tool types have been exercised locally.',
                    'Offline palette/image/math demo complete.')},
            ]))
            if args.question_only:
                script.write_text(json.dumps([
                    {'prefix': 'user: Run demo', 'calls': [
                        {'id': 'question', 'name': 'question', 'arguments': {'questions': [
                            {'id': 'notes', 'prompt': 'Any notes for the report?',
                             'options': [{'id': 'brief', 'label': 'Brief'}, {'id': 'detailed', 'label': 'Detailed'}]},
                            {'id': 'checks', 'prompt': 'Which evidence should be retained?',
                             'options': [{'id': 'data', 'label': 'Data'}, {'id': 'render', 'label': 'Rendering'}]},
                        ]}},
                    ]}, {'text': 'Offline question cursor demo complete.'},
                ]))
            # ToolCall.arguments is JSON bytes in the Go fixture, rather than a
            # string; RawMessage accepts an ordinary encoded object here.
            command = [binary, '--offline-script', str(script), '--workdir', str(project),
                       '--data-dir', str(root / 'data')]
        else:
            command = ['python3', str(Path(__file__).with_name('demo.py').resolve()), '--interactive',
                       '--visual-hold', '--binary', binary, '--artifacts-file', str(root / 'demo.json')]
        terminal = kitty.launch(command)

        def wait(needle, timeout=15, present=True):
            nonlocal last_text
            deadline = time.monotonic() + timeout
            while time.monotonic() < deadline:
                last_text = remote('get-text', '--extent', 'screen')
                (root / 'latest-screen.txt').write_text(last_text)
                if (needle in last_text) == present:
                    return last_text
                if terminal.poll() is not None:
                    raise RuntimeError('Kitty closed while waiting for ' + needle)
                time.sleep(0.05)
            raise AssertionError('screen missing ' + repr(needle))

        def wait_fullscreen():
            nonlocal last_text
            deadline = time.monotonic() + 10
            while time.monotonic() < deadline:
                last_text = remote('get-text', '--extent', 'screen')
                if ('Copy mode · updates paused · Esc returns' in last_text
                        and 'WORKSPACE' not in last_text and 'Context usage' not in last_text and '\n> ' not in last_text):
                    return last_text
                time.sleep(0.05)
            raise AssertionError('fullscreen left sidebar/composer')

        def send(text):
            remote('send-text', '--stdin', data=text.encode())

        def key(name):
            remote('send-key', name)

        def mouse(x, y, button=0):
            # Inject the SGR cell mouse input that Kitty normally sends to tcell.
            send(f'\x1b[<{button};{x};{y}M')
            if button == 0:
                send(f'\x1b[<0;{x};{y}m')

        def window_return(name, reset=False):
            if not args.tmux:
                return
            from PIL import Image, ImageChops
            def tmux(*arguments):
                result = subprocess.run(['tmux', '-S', str(tmux_sock), *arguments],
                                        capture_output=True, text=True, timeout=10)
                with (root / 'tmux-return.log').open('a') as log:
                    log.write(repr(arguments) + '\n' + result.stdout + result.stderr)
                result.check_returncode()
                return result.stdout.strip()
            original = tmux('display-message', '-p', '#{window_id}')
            # No terminal focus event is injected: tmux activation must work
            # even with the user's default focus-events=off.
            assert tmux('show-options', '-gv', 'focus-events') == 'off'
            capture(name + '-before')
            other = tmux('new-window', '-P', '-F', '#{window_id}', 'sleep 60')
            time.sleep(1)
            capture(name + '-away')
            if reset:
                # Reset only this private test terminal, modeling lost terminal
                # image data. A placeholder repaint alone cannot restore pixels.
                remote('action', 'clear_terminal reset active')
            tmux('select-window', '-t', original)
            time.sleep(1)
            capture(name + '-return')
            before = Image.open(root / (name + '-before.png')).convert('RGB')
            after = Image.open(root / (name + '-return.png')).convert('RGB')
            # Ignore status/footer and the blinking composer cursor.
            box = (60, 60, before.width - 60, before.height - 130)
            difference = ImageChops.difference(before.crop(box), after.crop(box))
            difference.save(root / (name + '-difference.png'))
            tmux('kill-window', '-t', other)
            assert difference.getbbox() is None, 'image changed after tmux window return: ' + name

        wait('/help')
        if args.question_only:
            send('Run demo\r')
            check_question_cursor(remote, wait, send, key, capture, root)
            key('enter')
            wait('Submit answers')
            capture('question-cursor-submit')
            key('enter')
            wait('Offline question cursor demo complete.')
            wait('Turn complete')
            with sqlite3.connect(root / 'data/history.sqlite') as db:
                result = json.loads(db.execute(
                    "SELECT result_json FROM tool_calls WHERE name='question'").fetchone()[0])
            (root / 'question-result.json').write_text(json.dumps(result, indent=2) + '\n')
            assert result['answers'] == [
                {'id': 'notes', 'values': ['Looks good.'], 'source': 'custom'},
                {'id': 'checks', 'values': ['data'], 'source': 'option'},
            ], result
            key('ctrl+c')
            terminal.wait(timeout=10)
            assert terminal.returncode == 0
            print(f'PASS: offline real Kitty question native cursor/edit/restore; tmux={args.tmux}: {root}')
            return
        check_composer(remote, wait, send, key, capture, root)
        if args.composer_only:
            key('ctrl+c')
            terminal.wait(timeout=10)
            assert terminal.returncode == 0
            print(f'PASS: real Kitty multiline composer; tmux={args.tmux}: {root}')
            return
        version = get_version()
        header = wait(f'TTC {version}').splitlines()[0]
        assert header.rstrip().endswith(f'TTC {version}'), (
            'version header is not at the top right')
        send('cursor draft')
        wait('> cursor draft')
        cursor_state = remote('get-text', '--extent', 'screen', '--add-cursor')
        (root / 'composer-cursor.txt').write_text(cursor_state)
        assert '\x1b[5 q' in cursor_state, 'composer cursor is not a blinking bar'
        capture('composer-cursor')
        for _ in 'cursor draft':
            key('backspace')
        send('Run demo\r')
        if args.offline:
            wait('Offline palette/image/math demo complete.')
            wait('Turn complete')
            wait('\U0010eeee', timeout=20)
            capture('markdown-math')

            def math_table_frame(text):
                # Kitty returns the actual placeholder codepoints/coordinates,
                # not pixels. Check these alongside real screenshots so a tall
                # image cannot silently erase the following source table row.
                rows = text.splitlines()
                header = next((i for i, row in enumerate(rows) if 'Quantity' in row), None)
                fraction = next((i for i, row in enumerate(rows) if 'Fraction' in row), None)
                matrix = next((i for i, row in enumerate(rows) if 'Matrix' in row), None)
                following = next((i for i, row in enumerate(rows) if 'Explicit' in row), None)
                assert header is not None and fraction is not None and matrix is not None and following is not None, text
                assert header < fraction < matrix < following, 'math table header/body rows reordered: ' + text
                header_cells = rows[header].split('│')
                assert '\U0010eeee' in header_cells[0], 'header q formula did not finish rendering: ' + text
                assert len(header_cells) >= 3 and 'Formula and context' in header_cells[1], 'math table header clipped: ' + text
                section = rows[fraction:following + 1]
                assert not any(token in '\n'.join(section) for token in (r'\frac', r'\begin', '[math unavailable]')), text
                image_rows = [i for i, row in enumerate(rows) if '\U0010eeee' in row and fraction <= i <= following]
                assert any(b == a + 1 for a, b in zip(image_rows, image_rows[1:])), 'no intact multirow formula in table: ' + text

                def separator_columns(row):
                    column, found = 0, []
                    for char in row:
                        if char == '│':
                            found.append(column)
                        if not unicodedata.combining(char):
                            column += 2 if unicodedata.east_asian_width(char) in ('W', 'F') else 1
                    return found

                separators = [separator_columns(row) for row in section]
                # A transcript scrollbar may add a third vertical rule; only
                # the two internal table separators belong to this check.
                assert separators and all(len(cols) >= 2 for cols in separators), 'math escaped three-column table: ' + text
                assert all(cols[:2] == separators[0][:2] for cols in separators), 'table rules shifted around tall formulas: ' + text
                assert separator_columns(rows[header])[:2] == separators[0][:2], 'table header rules misaligned: ' + text
                following_label = ''.join(row.split('│')[0].strip() for row in rows[following:following + 4])
                assert 'Explicitdisplay' in following_label.replace(' ', ''), 'following display row truncated: ' + text
                return text

            def capture_math_table(name):
                nonlocal last_text
                # Keep scrolling until the header AND following body rows fit;
                # stopping at Fraction clips the header just above the screen.
                # Small increments cannot skip a multirow source cell. Readiness
                # is checked afterwards because only visible assets are requested.
                for _ in range(120):
                    last_text = remote('get-text', '--extent', 'screen')
                    if all(label in last_text for label in ('Quantity', 'Formula and context', 'Fraction', 'Matrix', 'Explicit')):
                        break
                    mouse(20, 10, 64)
                    time.sleep(0.05)
                else:
                    raise AssertionError('math table and following rows not visible: ' + last_text)
                deadline = time.monotonic() + 20
                while True:
                    last_text = remote('get-text', '--extent', 'screen')
                    try:
                        math_table_frame(last_text)
                        break
                    except AssertionError:
                        if time.monotonic() >= deadline:
                            capture(name + '-failure')
                            raise
                        time.sleep(0.1)
                capture(name)

            # The shared research fixture's math table contains a fraction,
            # matrix, multiple formulas, styles and a following display cell.
            # Use a taller viewport so both tall cells and the next row fit.
            remote('resize-os-window', '--width', '120', '--height', '44', '--unit', 'cells')
            time.sleep(0.2)
            capture_math_table('math-inside-tables')
            remote('resize-os-window', '--width', '80', '--height', '44', '--unit', 'cells')
            time.sleep(0.2)
            for _ in range(30):
                key('ctrl+d')
            capture_math_table('math-inside-tables-narrow')
            remote('resize-os-window', '--width', '120', '--height', '38', '--unit', 'cells')
            time.sleep(0.2)
            for _ in range(30):
                key('ctrl+d')
            wait('Turn complete')
            key('ctrl+r')
            wait('Prompt history search')
            send('demo run')
            wait('Matches 1')
            capture('prompt-search-highlight')
            styled = remote('get-text', '--extent', 'screen', '--ansi')
            (root / 'prompt-search-highlight.ansi').write_text(styled)
            assert '\x1b[4' in styled or ';4' in styled, 'search matches are not underlined'
            key('esc')
            wait('Prompt history search', present=False)
            wait('Turn complete')
            for _ in range(30):
                key('ctrl+u')
                last_text = remote('get-text', '--extent', 'screen')
                if 'Sample' in last_text and 'Value' in last_text:
                    break
            assert 'Sample' in last_text and 'Value' in last_text
            capture('table-palette')
            for _ in range(30):
                key('ctrl+u')
                last_text = remote('get-text', '--extent', 'screen')
                if 'image_show' in last_text and '\U0010eeee' in last_text:
                    break
            assert 'image_show' in last_text and '\U0010eeee' in last_text
            assert 'System prompt' not in last_text, last_text
            capture('tools-palette')
            rows = last_text.splitlines()
            image_row = next(i for i,row in enumerate(rows,1) if '\U0010eeee' in row)
            mouse(10,image_row)
            wait('h/j/k/l pan')
            capture('image-preview')
            window_return('image-preview')
            window_return('image-preview-cache-loss', reset=True)
            # A terminal reset clears Kitty/tmux's negotiated keyboard mode.
            # Inject into the test pane so this check isolates image recovery.
            if args.tmux:
                subprocess.run(['tmux', '-S', str(tmux_sock), 'send-keys', 'Escape'], check=True)
            else:
                key('esc')
            wait('h/j/k/l pan', present=False)
            if args.tmux:
                subprocess.run(['tmux', '-S', str(tmux_sock), 'send-keys', 'C-c'], check=True)
            else:
                key('ctrl+c')
            terminal.wait(timeout=10)
            assert terminal.returncode == 0
            print(f'PASS: offline Kitty palette/image/math and highlighted multi-term search; tmux={args.tmux}: {root}')
            return
        pending_text = wait('awaiting read (0seg/0bytes) ...')
        capture('awaiting-tool')
        pending_row = next(i for i, row in enumerate(pending_text.splitlines(), 1) if 'awaiting read' in row)
        mouse(3, pending_row)
        wait('call_id')
        capture('awaiting-tool-inspector')
        demo_root = Path(json.loads((root / 'demo.json').read_text())['root'])
        (demo_root / 'project/stream.ready').touch()
        wait('Execution details')
        wait('Parameters:')
        capture('awaiting-tool-settled-details')
        key('esc')
        wait('┌', present=False)
        wait('● agent')
        wait('Fixture check')
        capture('running-agent')
        (demo_root / 'project/child.ready').touch()
        wait('● shell')
        wait('Running jobs · 2')  # The background shell and managed LSP are both live.
        wait('Timers · 1')
        wait('WORKSPACE')
        wait('demo')
        capture('live-sidebar')
        # While visual.ready holds main at the next tool boundary, both child
        # turns have settled. Inspect their compact response and returned answer
        # using actual rendered row coordinates, not database-only inspection.
        for _ in range(20):
            last_text = remote('get-text', '--extent', 'screen')
            if CHILD_PREFIX in last_text and '[Fixture check] assistant ' in last_text:
                try:
                    child_rows = compact_rows(last_text)
                    break
                except AssertionError:
                    if 'CHILD-HIDDEN-BODY' in last_text or CHILD_TAIL in last_text:
                        raise
            key('ctrl+u')
        else:
            raise AssertionError('compact child response/completion not visible: ' + last_text)
        capture('child-compact')
        remote('resize-os-window', '--width', '80', '--height', '38', '--unit', 'cells')
        deadline = time.monotonic() + 10
        while time.monotonic() < deadline:
            narrow_child = remote('get-text', '--extent', 'screen')
            try:
                assert max(map(len, narrow_child.splitlines())) <= 80, 'old wide frame'
                compact_rows(narrow_child, narrow=True)
                break
            except AssertionError:
                time.sleep(0.05)  # Wait for TTC's resize/reflow, not Kitty's cropped old frame.
        else:
            raise AssertionError('narrow response AND completion rows missing: ' + narrow_child)
        capture('child-compact-narrow')
        remote('resize-os-window', '--width', '120', '--height', '38', '--unit', 'cells')
        deadline = time.monotonic() + 10
        while time.monotonic() < deadline:
            last_text = remote('get-text', '--extent', 'screen')
            try:
                assert 'WORKSPACE' in last_text, 'old narrow frame'
                child_rows = compact_rows(last_text)
                break
            except AssertionError:
                time.sleep(0.05)
        else:
            raise AssertionError('wide compact rows not restored: ' + last_text)
        response, completion = child_rows
        mouse(8, response)
        time.sleep(0.2)
        direct_text = remote('get-text', '--extent', 'screen')
        assert '┌' not in direct_text, 'direct assistant row opened payload inspection'
        compact_rows(direct_text)
        capture('child-direct-no-inspect')
        mouse(8, completion)
        wait('Execution details')
        wait(CHILD_PREFIX)
        capture('child-completion-inspector-start')
        key('end')
        wait(CHILD_TAIL)
        capture('child-completion-inspector-tail')
        send('d')
        wait('Parameters:')
        capture('child-completion-details')
        key('esc')
        wait('┌', present=False)
        with sqlite3.connect(demo_root / 'data/history.sqlite') as db:
            child_entry, child_title = child_response_target(db)
            answers = [json.loads(row[0])['result'] for row in db.execute(
                "SELECT record_json FROM tool_records WHERE json_extract(record_json,'$.name')='subagent'")]
            verify_child_answers(db, answers)
        key('esc')  # Start the native-row search from the tail.
        for _ in range(80):
            last_text = remote('get-text', '--extent', 'screen')
            try:
                response_row = child_response_row(last_text, child_title)
                break
            except AssertionError:
                key('ctrl+u')
        else:
            raise AssertionError('native child response not visible: ' + last_text)
        capture('child-native-response-row')
        mouse(8, response_row)
        wait('R raw')
        capture('child-native-response-start')
        remote('resize-os-window', '--width', '80', '--height', '28', '--unit', 'cells')
        wait('Items:')  # Unchanged settings are absent from this request delta.
        time.sleep(0.15)
        capture('request-compact-narrow')
        remote('resize-os-window', '--width', '120', '--height', '38', '--unit', 'cells')
        # A pre-resize frame already contains the delta header. Wait for TTC's wider
        # border/footer before capturing expansion, not merely Kitty's canvas.
        deadline = time.monotonic() + 5
        while time.monotonic() < deadline:
            frame = remote('get-text', '--extent', 'screen')
            if max(map(len, frame.splitlines()), default=0) >= 110:
                break
            time.sleep(0.05)
        else:
            raise AssertionError('request inspector did not reflow to wide pane')
        send('e')
        wait('E compact')
        capture('request-expanded-start')
        send('e')
        wait('E expand')
        capture('request-compact-start')

        for _ in range(100):
            frame = remote('get-text', '--extent', 'screen')
            if 'Prettified raw JSON' in frame:
                capture('request-readable-then-json')
                break
            key('page_down')
            time.sleep(0.04)
        else:
            raise AssertionError('prettified native JSON section not reachable after readable view')

        def reveal_request_tail():
            nonlocal last_text
            for _ in range(240):
                last_text = remote('get-text', '--extent', 'screen')
                if CHILD_TAIL in last_text:
                    return
                key('page_down')
                time.sleep(0.06)
            capture('request-tail-failure')
            raise AssertionError('child tail not reachable in grouped request inspection')

        reveal_request_tail()
        capture('child-native-response-tail')
        send('r')
        wait('Exact raw JSON')
        capture('child-request-raw-start')
        send('n')  # Raw response is separate from the outgoing request.
        reveal_request_tail()
        capture('child-native-response-raw-tail')
        key('esc')
        wait('┌', present=False)
        send(f'/inspect {child_entry}\r')
        wait('┌')
        reveal_request_tail()
        capture('child-keyboard-inspector-tail')
        key('esc')
        wait('┌', present=False)
        # Return to the tail before continuing sidebar/focus regressions.
        for _ in range(20):
            key('ctrl+d')
        send('/help\r')
        wait('TTC help')
        capture('busy-help')
        key('esc')
        wait('Working')
        mouse(100, 2)
        wait('Working directory:')
        wait('Branch:')
        capture('workspace-inspector')
        key('esc')
        sidebar_text = wait('Context usage')
        context_row = next(i for i, row in enumerate(sidebar_text.splitlines(), 1) if 'Context usage' in row)
        mouse(100, context_row)
        wait('▸ Context usage')
        capture('sidebar-collapsed')
        key('ctrl+x'); key('f')
        text = wait_fullscreen()
        assert 'Copy mode · updates paused · Esc returns' in text, text
        capture('fullscreen-working')
        key('ctrl+x'); key('f')
        wait('Working')  # Normal status returns; copy mode does not stop the agent.
        mouse(100, context_row)
        remote('resize-os-window', '--width', '80', '--height', '20', '--unit', 'cells')
        key('ctrl+x'); key('s')
        sidebar_text = wait('Context usage')
        narrow_context_row = next(i for i, row in enumerate(sidebar_text.splitlines(), 1) if 'Context usage' in row)
        # Small panes expose only a few context rows; scroll within that list
        # until its run-wide output totals are visible rather than assuming one notch.
        for _ in range(12):
            if 'Run output · all agents' in remote('get-text', '--extent', 'screen'):
                break
            mouse(70, narrow_context_row + 1, 65)
            time.sleep(0.1)
        wait('Run output · all agents')
        capture('sidebar-narrow-scroll')
        key('esc')
        remote('resize-os-window', '--width', '120', '--height', '38', '--unit', 'cells')
        demo_root = Path(json.loads((root / 'demo.json').read_text())['root'])
        (demo_root / 'project/visual.ready').touch()
        question_text = wait('Yes (Recommended)')
        assert question_text.index('Finish the Markdown report?') < question_text.index('Yes (Recommended)')
        assert question_text.index('Other · free-text input') < question_text.index('Up/Down choose')
        assert question_text.index('Up/Down choose') < question_text.index('←/→ tabs · Esc dismisses')
        capture('question')
        key('esc')
        wait('What would you like to do instead?')
        capture('question-dismissed')
        send('/questions\r')
        wait('Yes (Recommended)')
        key('enter')
        wait('Any notes for the report?')
        check_question_cursor(remote, wait, send, key, capture, root)
        key('enter')
        review_text = wait('Submit answers')
        for item in ('1. Finish the Markdown report?', '2. Any notes for the report?', '3. Which evidence should be retained?'):
            assert item in review_text, review_text
        assert review_text.index('Submit answers') < review_text.index('Enter submits')
        capture('submit')
        key('enter')
        deadline = time.monotonic() + 15
        markdown = (demo_root / 'project/markdown.md').read_text()
        while not demo_completed(demo_root / 'data/history.sqlite', markdown):
            assert time.monotonic() < deadline, 'main turn did not durably complete'
            time.sleep(0.05)
        wait('Turn complete')
        wait('\U0010eeee', timeout=20)  # Actual formula placeholders, after async rendering.
        deadline = time.monotonic() + 20
        while r'\frac' in last_text or r'\bar' in last_text:
            assert time.monotonic() < deadline, 'formulas did not finish rendering'
            time.sleep(0.1)
            last_text = remote('get-text', '--extent', 'screen')
        wait('20 tool types')
        capture('markdown-math')
        key('ctrl+x'); key('f')
        text = wait_fullscreen()
        capture('fullscreen-math')
        key('ctrl+x'); key('f')
        for _ in range(12):
            key('ctrl+u')
            last_text = remote('get-text', '--extent', 'screen')
            if 'Sample' in last_text and 'Value' in last_text and 'Unit' in last_text:
                break
        assert 'Sample' in last_text and 'Value' in last_text, 'table was not visible'
        capture('table-headers')
        for _ in range(80):
            key('ctrl+u')
            last_text = remote('get-text', '--extent', 'screen')
            direct_glob = any(row.lstrip().startswith('glob ·') and '**/*.py' in row
                              for row in last_text.splitlines())
            if direct_glob and '\U0010eeee' in last_text:
                break
        assert direct_glob and '\U0010eeee' in last_text, 'direct glob briefing and image thumbnail were not visible'
        capture('tool-briefings')
        thumbnail = remote('get-text', '--extent', 'screen').splitlines()
        image_row = next(i for i, row in enumerate(thumbnail) if '\U0010eeee' in row)
        mouse(10, image_row + 1)
        wait('h/j/k/l pan')
        capture('image-preview-fit')
        send('lj+')
        wait('Zoom 125%')
        mouse(60, 18)
        wait('pixel (')
        capture('image-preview-point')
        key('enter')
        key('esc')  # Follow the new notification turn at the tail.
        wait('Image point confirmed.')
        capture('image-click-notification')
        # Inspect the exact recorded prompt using a local command. No extra
        # inference request is needed; the saved audit stays explicitly inspectable.
        demo_root = Path(json.loads((root / 'demo.json').read_text())['root'])
        with sqlite3.connect(demo_root / 'data/history.sqlite') as db:
            prompt = db.execute("SELECT id FROM entries WHERE json_extract(content_json,'$.type')='system_prompt' ORDER BY id LIMIT 1").fetchone()[0]
            shell = db.execute("SELECT r.entry_id FROM tool_records r JOIN tool_calls c ON c.id=r.call_id WHERE c.name='shell' AND json_extract(c.result_json,'$.exit_code')=0 ORDER BY r.entry_id LIMIT 1").fetchone()[0]
            job_read = db.execute("SELECT r.entry_id FROM tool_records r JOIN tool_calls c ON c.id=r.call_id WHERE c.name='job_read' ORDER BY r.entry_id LIMIT 1").fetchone()[0]
        send(f'/inspect {shell}\r')
        wait('Command:')
        capture('shell-details')
        key('end')
        capture('shell-output')
        key('esc')
        # Wait for bare Escape to cross tmux before sending a leading slash;
        # otherwise the next command can be decoded as an Alt+/ key sequence.
        wait('Esc closes', present=False)
        send(f'/inspect {job_read}\r')
        wait('Parameters:')
        key('end')
        result_text = wait('Output:')
        assert 'background evidence' in result_text, result_text
        assert 'showing tail' not in result_text, result_text
        capture('job-read-output')
        key('esc')
        wait('Esc closes', present=False)
        send(f'/inspect {prompt}\r')
        wait('You are TTC')
        capture('system-prompt')
        key('end')
        capture('system-prompt-end')
        key('esc')
        wait('Esc closes', present=False)
        send('/rename Long session title for scrolling TAIL-SEEN\r')
        wait('TAIL-SEEN')  # A local rename emits no transcript title; this is the scrolling sidebar tail.
        capture('sidebar-title-tail')
        key('ctrl+c')
        terminal.wait(timeout=10)
        if terminal.returncode != 0:
            raise AssertionError(f'Kitty exited {terminal.returncode}')
        print(f'PASS: real Kitty sidebar/fullscreen/images/confirmed click/formula/table/viewer captures: {root}')
    except BaseException:
        (root / 'failure.txt').write_text(traceback.format_exc())
        print(f'Failure context saved: {root}')
        raise
    finally:
        kitty.close()


if __name__ == '__main__':
    main()

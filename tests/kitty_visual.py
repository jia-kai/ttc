#!/usr/bin/env python3
"""Capture the real Kitty TUI under Xvfb against TTC's local mock server.

Requires kitty (with remote-control screenshot), Xvfb, and a software OpenGL
implementation. Saves screenshots, terminal text, and complete failure logs.
"""
import argparse
import json
import os
from pathlib import Path
import signal
import shutil
import sqlite3
import subprocess
import tempfile
import time
import traceback


from scratch import private_scratch

def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary', default='./ttc')
    parser.add_argument('--tmux', action='store_true', help='Test auto-detection through a private tmux with Kitty identity cleared')
    parser.add_argument('--offline', action='store_true', help='Use a short scripted palette/image/math fixture without a TCP server')
    args = parser.parse_args()
    scratch = private_scratch()
    root = Path(tempfile.mkdtemp(prefix='kitty-visual-', dir=scratch))
    sock = root / 'control.sock'
    env = dict(os.environ, LIBGL_ALWAYS_SOFTWARE='1')
    env.pop('TMUX', None)  # This is a new Kitty PTY, outside the caller's tmux.
    env.pop('NO_COLOR', None)  # Capture the normal colored UI, independent of CI preferences.
    read_fd, write_fd = os.pipe()
    xvfb_log = (root / 'xvfb.log').open('wb')
    kitty_log = (root / 'kitty.log').open('wb')
    xvfb = subprocess.Popen(['Xvfb', '-displayfd', str(write_fd), '-screen', '0', '1280x900x24',
                             '-nolisten', 'tcp'], pass_fds=[write_fd], stdout=xvfb_log, stderr=xvfb_log)
    os.close(write_fd)
    terminal = None
    tmux_sock = root / 'tmux.sock'
    tmux_config = root / 'tmux.conf'
    tmux_config.write_text('set -g default-terminal tmux-256color\nset -g allow-passthrough on\nset -as terminal-features ",xterm-kitty:RGB"\n')
    last_text = ''
    try:
        # displayfd reports readiness and an available display; no fixed display
        # number or interference with the user's desktop/session is needed.
        import select
        if not select.select([read_fd], [], [], 10)[0]:
            raise RuntimeError('Xvfb did not become ready')
        display = os.read(read_fd, 64).decode().strip()
        if not display.isdigit():
            raise RuntimeError('Xvfb failed to create a display; see xvfb.log')
        env['DISPLAY'] = ':' + display
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
            # ToolCall.arguments is JSON bytes in the Go fixture, rather than a
            # string; RawMessage accepts an ordinary encoded object here.
            command = [binary, '--offline-script', str(script), '--workdir', str(project),
                       '--data-dir', str(root / 'data'), '--auto-name=false']
        else:
            command = ['python3', str(Path(__file__).with_name('demo.py').resolve()), '--interactive',
                       '--visual-hold', '--binary', binary, '--artifacts-file', str(root / 'demo.json')]
        if args.tmux:
            command = ['tmux', '-S', str(tmux_sock), '-f', str(tmux_config), 'new-session',
                       'env', '-u', 'KITTY_WINDOW_ID',
                       'COLORTERM=truecolor', *command]
        terminal = subprocess.Popen([
            'kitty', '--config', 'NONE', '-o', 'linux_display_server=x11',
            '-o', 'allow_remote_control=socket-only', '-o', 'enable_audio_bell=no',
            '-o', 'remember_window_size=no', '-o', 'initial_window_width=120c',
            '-o', 'initial_window_height=38c', '-o', 'font_size=11',
            '--listen-on', f'unix:{sock}', *command,
        ], env=env, stdout=kitty_log, stderr=kitty_log, start_new_session=True)

        def remote(command, *arguments, data=None):
            result = subprocess.run(['kitten', '@', '--to', f'unix:{sock}', command, *arguments],
                                    env=env, input=data, stdout=subprocess.PIPE,
                                    stderr=subprocess.PIPE, timeout=10)
            with (root / 'remote.log').open('ab') as log:
                log.write((command + ' ' + repr(arguments) + '\n').encode())
                log.write(result.stdout + result.stderr)
            result.check_returncode()
            return result.stdout.decode()

        deadline = time.monotonic() + 15
        while not sock.exists():
            if terminal.poll() is not None or time.monotonic() > deadline:
                raise RuntimeError('Kitty did not create its control socket')
            time.sleep(0.05)

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
                if 'WORKSPACE' not in last_text and 'Context usage' not in last_text and '\n> ' not in last_text:
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

        def capture(name):
            remote('screenshot', str(root / (name + '.png')))
            (root / (name + '.txt')).write_text(remote('get-text', '--extent', 'screen'))

        wait('/help')
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
                if 'image_show' in last_text and '\U0010eeee' in last_text and 'System prompt' in last_text:
                    break
            assert 'image_show' in last_text and '\U0010eeee' in last_text
            assert last_text.count('System prompt') == 1, last_text
            capture('tools-palette')
            rows = last_text.splitlines()
            image_row = next(i for i,row in enumerate(rows,1) if '\U0010eeee' in row)
            mouse(10,image_row)
            wait('h/j/k/l pan')
            capture('image-preview')
            key('esc')
            wait('h/j/k/l pan', present=False)
            key('ctrl+c')
            terminal.wait(timeout=10)
            assert terminal.returncode == 0
            print(f'PASS: offline Kitty palette/image/math and highlighted multi-term search; tmux={args.tmux}: {root}')
            return
        pending_text = wait('awaiting read ...')
        capture('awaiting-tool')
        pending_row = next(i for i, row in enumerate(pending_text.splitlines(), 1) if 'awaiting read' in row)
        mouse(3, pending_row)
        wait('call_id')
        capture('awaiting-tool-inspector')
        key('esc')
        demo_root = Path(json.loads((root / 'demo.json').read_text())['root'])
        (demo_root / 'project/stream.ready').touch()
        wait('● agent')
        wait('Independent fixture check')
        capture('running-agent')
        (demo_root / 'project/child.ready').touch()
        wait('● shell')
        wait('Running jobs · 1')
        wait('Timers · 1')
        wait('WORKSPACE')
        wait('demo')
        capture('live-sidebar')
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
        assert 'Working' in text
        capture('fullscreen-working')
        key('ctrl+x'); key('f')
        mouse(100, context_row)
        remote('resize-os-window', '--width', '80', '--height', '20', '--unit', 'cells')
        key('ctrl+x'); key('s')
        wait('Context usage')
        mouse(70, 10, 65)  # Wheel down in the context list, independent of jobs/timers.
        wait('Tool results')
        capture('sidebar-narrow-scroll')
        key('esc')
        remote('resize-os-window', '--width', '120', '--height', '38', '--unit', 'cells')
        demo_root = Path(json.loads((root / 'demo.json').read_text())['root'])
        (demo_root / 'project/visual.ready').touch()
        wait('Yes (Recommended)')
        capture('question')
        key('enter')
        wait('Any notes for the report?')
        key('down')
        key('down')
        key('enter')
        send('Looks good.')
        key('enter')
        key('right')
        wait('Which evidence should be retained?')
        key('enter')
        wait('Submit answers')
        capture('submit')
        key('enter')
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
        for _ in range(20):
            key('ctrl+u')
            last_text = remote('get-text', '--extent', 'screen')
            if 'glob' in last_text and '**/*.py' in last_text:
                break
        assert 'glob' in last_text and '**/*.py' in last_text, 'compact glob briefing was not visible'
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
        # inference request is needed, and the placeholder remains inspectable.
        demo_root = Path(json.loads((root / 'demo.json').read_text())['root'])
        with sqlite3.connect(demo_root / 'data/history.sqlite') as db:
            prompt = db.execute("SELECT id FROM entries WHERE json_extract(content_json,'$.type')='system_prompt' ORDER BY id LIMIT 1").fetchone()[0]
            shell = db.execute("SELECT r.entry_id FROM tool_records r JOIN tool_calls c ON c.id=r.call_id WHERE c.name='shell' AND json_extract(c.result_json,'$.exit_code')=0 ORDER BY r.entry_id LIMIT 1").fetchone()[0]
        send(f'/inspect {shell}\r')
        wait('Command:')
        capture('shell-details')
        key('end')
        capture('shell-output')
        key('esc')
        send(f'/inspect {prompt}\r')
        wait('You are TTC')
        capture('system-prompt')
        key('end')
        capture('system-prompt-end')
        key('esc')
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
        os.close(read_fd)
        if terminal is not None and terminal.poll() is None:
            os.killpg(terminal.pid, signal.SIGTERM)
            try:
                terminal.wait(timeout=5)
            except subprocess.TimeoutExpired:
                os.killpg(terminal.pid, signal.SIGKILL)
                terminal.wait()
        if args.tmux and tmux_sock.exists():
            subprocess.run(['tmux', '-S', str(tmux_sock), 'kill-server'],
                           stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=5)
        xvfb.terminate()
        try:
            xvfb.wait(timeout=5)
        except subprocess.TimeoutExpired:
            xvfb.kill()
            xvfb.wait()
        xvfb_log.close()
        kitty_log.close()


if __name__ == '__main__':
    main()

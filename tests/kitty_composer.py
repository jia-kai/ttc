"""Focused composer checks shared by the real Kitty screenshot suites."""
import json
import re
import shlex
import time


def configure_editor(env, root):
    """Record drafts without changing them, using the normal external editor path."""
    editor = root / 'composer-editor.py'
    editor.write_text(
        'from pathlib import Path\nimport sys\n'
        "Path(__file__).with_name('composer-source.txt').write_bytes(Path(sys.argv[-1]).read_bytes())\n")
    env['VISUAL'] = 'python3 ' + shlex.quote(str(editor))


def check_composer(remote, wait, send, key, capture, root):
    def cursor(visible, name):
        deadline = time.monotonic() + 10
        while time.monotonic() < deadline:
            state = remote('get-text', '--extent', 'screen', '--add-cursor')
            (root / (name + '-cursor.txt')).write_text(state)
            if ('\x1b[?25h' if visible else '\x1b[?25l') in state:
                return
            time.sleep(0.05)
        raise AssertionError('incorrect composer cursor visibility: ' + name)

    def exact_source(expected):
        snapshot = root / 'composer-source.txt'
        snapshot.unlink(missing_ok=True)
        key('ctrl+x')
        send('e')
        deadline = time.monotonic() + 10
        while not snapshot.exists() and time.monotonic() < deadline:
            time.sleep(0.05)
        assert snapshot.read_text() == expected, snapshot.read_text()
        wait(expected.splitlines()[-1][-20:])

    # A local command creates history without consuming the scripted provider.
    send('/help\r')
    wait('TTC help')
    key('esc')
    wait('TTC help', present=False)
    soft = 'SOFT-' + 'w' * 180 + '-TAIL'
    send(soft)
    screen = wait('-TAIL')
    rows = screen.splitlines()
    start = next(i for i, row in enumerate(rows) if 'SOFT-' in row)
    end = next(i for i, row in enumerate(rows) if '-TAIL' in row)
    assert 1 <= end - start <= 5, 'long input did not soft-wrap: ' + screen
    capture('composer-soft-wrap')
    exact_source(soft)  # Display wrapping must not insert source newlines.
    key('ctrl+c')
    wait('SOFT-', present=False)

    lines = [f'COMPOSER-{i:02}' for i in range(1, 10)]
    send('\x1b[200~' + '\n'.join(lines[:3]) + '\x1b[201~')
    screen = wait(lines[2])
    positions = [next(i for i, row in enumerate(screen.splitlines()) if line in row) for line in lines[:3]]
    assert positions == list(range(positions[0], positions[0] + 3)), screen
    capture('composer-three-rows')
    send('\x1b[200~\n' + '\n'.join(lines[3:]) + '\x1b[201~')
    screen = wait(lines[8])
    visible = re.findall(r'COMPOSER-\d\d', screen)
    assert visible == lines[3:], 'composer did not cap at six rows: ' + screen
    cursor(True, 'composer-bottom')
    capture('composer-six-row-cap')

    key('ctrl+k')
    screen = wait(lines[2])
    assert lines[8] not in screen and lines[7] in screen, screen
    cursor(False, 'composer-scroll-up')
    capture('composer-scroll-up-hidden-cursor')
    key('ctrl+j')
    wait(lines[8])
    cursor(True, 'composer-scroll-down')
    capture('composer-scroll-down')
    for _ in range(3):
        key('ctrl+k')
    wait(lines[0])
    cursor(False, 'composer-scroll-top')
    key('left')
    wait(lines[8])
    cursor(True, 'composer-movement-reveal')
    key('right')
    for _ in range(3):
        key('ctrl+k')
    wait(lines[0])
    cursor(False, 'composer-before-edit')
    send('!')
    wait(lines[8] + '!')
    cursor(True, 'composer-edit-reveal')
    capture('composer-edit-reveal')
    expected = '\n'.join(lines) + '!'
    exact_source(expected)  # Scroll keys must neither cut nor insert newlines.

    # Even from multiline input, one Up recalls history, one Down restores draft.
    key('up')
    screen = wait('> /help')
    assert 'COMPOSER-' not in screen, screen
    key('down')
    wait(lines[8] + '!')
    capture('composer-history-restored')
    exact_source(expected)

    remote('resize-os-window', '--width', '120', '--height', '6', '--unit', 'cells')
    deadline = time.monotonic() + 10
    while time.monotonic() < deadline:
        screen = remote('get-text', '--extent', 'screen')
        visible = re.findall(r'COMPOSER-\d\d', screen)
        if len(screen.splitlines()) <= 6 and 1 <= len(visible) <= 3 and lines[8] in visible:
            break
        time.sleep(0.05)
    else:
        raise AssertionError('composer did not clamp to short terminal: ' + screen)
    capture('composer-short-terminal')
    remote('resize-os-window', '--width', '120', '--height', '38', '--unit', 'cells')
    wait(lines[3])
    key('ctrl+c')
    wait('COMPOSER-', present=False)

    # Exercise Kitty/tmux transport, event batching and layout, not just insert.
    # Timings are diagnostic: normal harness timeouts detect hangs, while no
    # tight latency threshold depends on host load or remote-control overhead.
    large = ('LARGE-PASTE-' + '0123456789abcdef' * 4096)[:65536]
    large += '\nLARGE-PASTE-LINE-2\nLARGE-PASTE-END'
    started = time.monotonic()
    send('\x1b[200~' + large + '\x1b[201~')
    wait('LARGE-PASTE-END')
    settled = time.monotonic()
    exact_source(large)
    checked = time.monotonic()
    (root / 'composer-large-source.txt').write_bytes((root / 'composer-source.txt').read_bytes())
    timings = {
        'payload_bytes': len(large.encode()),
        'ascii_body_characters': 65536,
        'send_to_visible_tail_seconds': settled - started,
        'editor_roundtrip_seconds': checked - settled,
        'send_to_verified_source_seconds': checked - started,
    }
    (root / 'composer-large-paste-timing.json').write_text(json.dumps(timings, indent=2) + '\n')
    capture('composer-large-paste')
    key('ctrl+c')
    wait('LARGE-PASTE-END', present=False)

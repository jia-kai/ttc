"""Real Kitty question editing checks, shared by mock and offline workflows."""
import re
import time


def check_question_cursor(remote, wait, send, key, capture, root):
    """Exercise Other, then leave the fixture's original notes answer in place."""
    def cursor(name, text='abcdef', offset=3, visible=True):
        deadline = time.monotonic() + 10
        while time.monotonic() < deadline:
            state = remote('get-text', '--extent', 'screen', '--add-cursor')
            (root / (name + '-cursor.txt')).write_text(state)
            plain = remote('get-text', '--extent', 'screen')
            (root / (name + '.txt')).write_text(plain)
            positions = re.findall(r'\x1b\[(\d+);(\d+)H', state)
            if not visible:
                ready = '\x1b[?25l' in state
            else:
                rows = plain.splitlines()
                matches = [(row, line.index(text)) for row, line in enumerate(rows) if text in line]
                # Check both native visibility and its actual cell, not merely
                # the composer cursor elsewhere on the screen. ASCII fixture
                # text makes Python columns identical to terminal cell columns.
                ready = ('\x1b[?25h' in state and len(matches) == 1 and positions
                         and tuple(map(int, positions[-1])) ==
                         (matches[0][0] + 1, matches[0][1] + offset + 1))
                ready = ready and text in state and '▏' not in plain
            if ready:
                capture(name)
                return
            time.sleep(0.05)
        raise AssertionError('incorrect native question cursor: ' + name + '\n' + repr(state))

    wait('Any notes for the report?')
    wait('Ctrl+U/D context')
    initial = remote('get-text', '--extent', 'screen')
    (root / 'question-inline-context.txt').write_text(initial)
    rows = initial.splitlines()
    question_rows = [row for row, text in enumerate(rows) if 'Any notes for the report?' in text]
    assert question_rows and question_rows[-1] >= len(rows) // 2, initial
    # The conversation remains rendered above the form, rather than hidden
    # under the former modal. Both offline and mock fixtures start this way.
    assert any('Run demo' in text or 'Waiting for answer' in text
               for text in rows[:len(rows) // 2]), initial
    key('down')
    key('down')
    key('enter')  # Other enters the shared free-text composer.
    wait('Esc leaves text entry')
    send('abcdef')
    wait('abcdef')
    for _ in range(3):
        key('left')
    cursor('question-cursor-middle')
    send('X')
    wait('abcXdef')
    cursor('question-cursor-insert', 'abcXdef', 4)
    key('backspace')
    wait('abcdef')
    key('delete')
    wait('abcef')
    send('d')
    wait('abcdef')
    key('left')
    cursor('question-cursor-edited')

    # Leaving editing hides the terminal cursor; tab navigation and reopening
    # the pending dialog must preserve both the draft and insertion position.
    key('esc')
    wait('Up/Down choose')
    cursor('question-cursor-not-editing', visible=False)
    key('tab')
    wait('Which evidence should be retained?')
    key('shift+tab')
    wait('Any notes for the report?')
    key('enter')
    cursor('question-cursor-tab-restored')
    key('esc')
    wait('Up/Down choose')
    cursor('question-cursor-before-dismiss', visible=False)
    key('esc')
    wait('What would you like to do instead?')
    send('/questions\r')
    wait('Any notes for the report?')
    key('enter')
    cursor('question-cursor-reopened')

    # Keep existing server-side fixture answers unchanged.
    key('ctrl+a')
    for _ in 'abcdef':
        key('delete')
    wait('abcdef', present=False)
    send('Looks good.')
    wait('Looks good.')
    cursor('question-cursor-final-notes', 'Looks good.', len('Looks good.'))
    key('enter')
    wait('Which evidence should be retained?')

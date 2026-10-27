#!/usr/bin/env python3
"""Exercise `ttc FILE` in real headless Kitty, optionally through private tmux.

Requires Xvfb, kitty/kitten (including remote-control screenshot), Pillow, and
Node/npm plus rsvg-convert for MathJax. The normal MathJax cache is reused; no credentials,
history, mock provider or inference are used. All artifacts survive cleanup.
"""
import argparse
import json
from pathlib import Path
import struct
import zlib
import tempfile
import time
import traceback
import unicodedata

from scratch import private_scratch
from kitty_terminal import KittyTerminal


PLACEHOLDER = '\U0010eeee'
FIXTURE = r"""# Markdown layout probe

Fullwidth probe: a deliberately long line crosses the space normally reserved for the session sidebar and reaches the right side.

- outer-lead

  outer-tail

  - nested-lead

    $$
    \begin{aligned}
    a &= \frac{1}{1+x} \\
    b &= \sum_{k=1}^{n} k
    \end{aligned}
    $$

    nested-tail

  outer-after

> quote-lead
>
> $$
> q = \frac{1}{1+y}
> $$
>
> quote-tail

Inline-lead $\frac{\frac{a}{b}}{\frac{c}{d}}$ inline-tail.

| Plain name | Plain value |
| ---------- | ----------- |
| plain-row  | plain-tail  |

| Math name | Math value                |
| --------- | ------------------------- |
| math-row  | $\frac{1}{1+z}$ math-tail |

Layout-end.

"""

IMAGE_FIXTURE = r"""## Local image probe

image-before ![local figure](plot%20sample.png) image-after $x^2$ image-end.

Image formula $\frac{1}{1+x}$ formula-tail.

- image-list ![list figure](plot%20sample.png) list-image-tail.

| Image cell                                  | Context          |
| ------------------------------------------- | ---------------- |
| ![table figure](plot%20sample.png) table-end | image-table-tail |

![missing-figure](absent.png)

![remote-figure](https://example.invalid/never-fetch.png)

Image-probe-end.

"""


def write_png(path):
    """Make a colorful asymmetric PNG without requiring an imaging library."""
    def chunk(kind, data):
        return struct.pack('!I', len(data)) + kind + data + struct.pack('!I', zlib.crc32(kind + data))
    pixels = b''.join(b'\x00' + bytes(value for x in range(192)
                                    for value in (x, y * 2, (x + y) % 256))
                      for y in range(96))
    path.write_bytes(b'\x89PNG\r\n\x1a\n' + chunk(b'IHDR', struct.pack('!2I5B', 192, 96, 8, 2, 0, 0, 0))
                     + chunk(b'IDAT', zlib.compress(pixels)) + chunk(b'IEND', b''))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary', default='./ttc')
    parser.add_argument('--tmux', action='store_true',
                        help='Use a private tmux socket/config with graphics passthrough')
    args = parser.parse_args()
    binary = str(Path(args.binary).resolve())
    root = Path(tempfile.mkdtemp(prefix='kitty-markdown-', dir=private_scratch()))
    print(f'Artifacts: {root}', flush=True)
    document_dir = root / 'document'
    document_dir.mkdir()
    write_png(document_dir / 'plot sample.png')
    fixture = document_dir / 'markdown-layout.md'
    paragraphs = [f'Scroll paragraph {i:03d}: retained document content for half-page navigation.\n\n'
                  for i in range(100)]
    fixture.write_text(FIXTURE + ''.join(paragraphs[:20]) + IMAGE_FIXTURE + ''.join(paragraphs[20:]))
    kitty = KittyTerminal(root, tmux=args.tmux)
    kitty.env.update(XDG_CONFIG_HOME=str(root / 'config'), XDG_DATA_HOME=str(root / 'data'))
    kitty.env.pop('TCELL_TRUECOLOR', None)
    with kitty.tmux_config.open('a') as config:
        config.write('set -g status off\n')
    remote = kitty.remote
    terminal = None

    def screen():
        text = remote('get-text', '--extent', 'screen')
        (root / 'latest-screen.txt').write_text(text)
        return text

    def wait(check, description, timeout=30):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            text = screen()
            if check(text):
                return text
            if terminal.poll() is not None:
                raise RuntimeError('Kitty exited while waiting for ' + description)
            time.sleep(0.1)
        raise AssertionError('Timed out waiting for ' + description)

    def capture(name):
        kitty.capture(name)
        assert (root / (name + '.png')).stat().st_size > 1000, 'empty screenshot'

    def clean_ui(text):
        assert not any(s in text for s in (
            'WORKSPACE', 'Context usage', '/help', '\n> ', 'Send a message',
            '[math unavailable]', '[math error]', r'\frac', r'\begin', '$$',
        )), 'session UI or literal math leaked into document:\n' + text

    def column(row, token):
        # Placeholder coordinates are combining marks, not terminal cells.
        return sum(0 if unicodedata.combining(c) else
                   2 if unicodedata.east_asian_width(c) in ('W', 'F') else 1
                   for c in row[:row.index(token)])

    def layout_ready(text):
        return all(token in text for token in (
            'Markdown layout probe', 'outer-lead', 'outer-tail', 'nested-lead',
            'nested-tail', 'outer-after', 'quote-tail', 'inline-tail',
            'plain-tail', 'math-tail', 'Layout-end.', PLACEHOLDER,
        )) and not any(token in text for token in (r'\frac', r'\begin', '$$'))

    def check_layout(text):
        clean_ui(text)
        rows = text.splitlines()
        positions = {}
        for token in ('outer-lead', 'outer-tail', 'nested-lead', 'nested-tail', 'outer-after'):
            i, row = next((i, row) for i, row in enumerate(rows) if token in row)
            positions[token] = (i, column(row, token))
        assert positions['outer-lead'][1] == positions['outer-tail'][1] == positions['outer-after'][1], text
        assert positions['nested-lead'][1] == positions['nested-tail'][1] > positions['outer-lead'][1], text
        first, last = positions['nested-lead'][0], positions['nested-tail'][0]
        assert any(PLACEHOLDER in row for row in rows[first + 1:last]), 'nested display math missing'
        assert positions['outer-tail'][0] < first < last < positions['outer-after'][0], text
        quote_start = next(i for i, row in enumerate(rows) if 'quote-lead' in row)
        quote_end = next(i for i, row in enumerate(rows) if 'quote-tail' in row)
        assert any(PLACEHOLDER in row for row in rows[quote_start + 1:quote_end]), 'quoted math missing'
        assert any(PLACEHOLDER in row for row in rows if 'math-row' in row), 'math table not rendered'
        assert 'the right side.' in ' '.join(text.split()), 'full-width paragraph was clipped'
        return positions

    def click(x, y):
        remote('send-text', '--stdin', data=f'\x1b[<0;{x + 1};{y + 1}M\x1b[<0;{x + 1};{y + 1}m'.encode())

    def image_frame(text):
        return all(token in text for token in ('Local image probe', 'image-before', 'image-after',
                   'formula-tail', 'image-list', 'list-image-tail', 'table-end',
                   'image-table-tail', 'missing-figure', 'remote-figure', 'Image-probe-end.'))

    def image_rows(text):
        rows = text.splitlines()
        start = next(i for i, row in enumerate(rows) if 'Local image probe' in row)
        end = next(i for i, row in enumerate(rows) if 'Image-probe-end.' in row)
        return rows, start, end

    def plain(text):
        return ''.join(c for c in text if not unicodedata.combining(c))

    def check_images():
        for _ in range(6):
            text = screen()
            if 'image-before' in text:
                text = wait(lambda text: 'image-before' not in text or (
                            any('image-before' in row and row.count(PLACEHOLDER) > 10
                                for row in text.splitlines()) and '[loading image:' not in text
                            and r'\frac' not in text), 'visible image section assets')
            if image_frame(text):
                break
            remote('send-key', 'ctrl+d')
            time.sleep(0.2)
        else:
            raise AssertionError('image section did not fit on screen')
        text = wait(lambda text: image_frame(text) and any(
            'image-before' in row and row.count(PLACEHOLDER) > 10 for row in text.splitlines()),
            'relative escaped-space PNG rendered')
        rows, start, end = image_rows(text)
        clean_ui(text)
        image_row = next(i for i in range(start, end) if 'image-before' in rows[i])
        formula_row = next(i for i in range(start, end) if 'formula-tail' in rows[i])
        assert PLACEHOLDER in rows[formula_row], 'adjacent formula did not render'
        list_row = next(i for i in range(start, end) if 'image-list' in rows[i])
        table_row = next(i for i in range(start, end) if 'table-end' in rows[i])
        assert PLACEHOLDER in rows[list_row], 'list image did not render'
        # A tall table image can end above the baseline of its following text.
        table_start = next(i for i in range(start, table_row) if 'Image cell' in rows[i])
        assert any(PLACEHOLDER in row for row in rows[table_start:table_row + 1]), 'table image missing'
        for label in ('[image unavailable: missing-figure', 'Image: remote-figure'):
            assert any(label in row and PLACEHOLDER not in row for row in rows), 'fallback is not labeled: ' + label
        capture('images-document')
        text = screen()
        (root / 'images-before-preview.txt').write_text(text)
        for row, token in ((image_row, 'image-before'), (formula_row, PLACEHOLDER)):
            click(column(rows[row], token), row)
            time.sleep(0.2)
            assert 'h/j/k/l pan' not in screen(), 'text/formula click opened image preview'
        click(column(rows[image_row], PLACEHOLDER) + 1, image_row)
        preview = wait(lambda text: 'h/j/k/l pan' in text and 'source 192x96' in text,
                       'image enlargement with original source dimensions')
        assert '[ OK' not in preview, 'document image requested point confirmation'
        capture('image-preview-fit')
        remote('send-text', '--stdin', data=b'+')
        wait(lambda text: 'Zoom 125%' in text, 'image preview zoom')
        capture('image-preview-zoom')
        remote('send-text', '--stdin', data=b'l')
        time.sleep(0.2)
        capture('image-preview-pan')
        from PIL import Image, ImageChops
        with Image.open(root / 'image-preview-zoom.png') as before, Image.open(root / 'image-preview-pan.png') as after:
            # Ignore the frame/footer: actual canvas pixels must move when panned.
            box = (before.width // 4, before.height // 4, before.width * 3 // 4, before.height * 3 // 4)
            assert ImageChops.difference(before.convert('RGB').crop(box), after.convert('RGB').crop(box)).getbbox(), 'pan did not change image pixels'
        remote('send-key', 'esc')
        wait(lambda returned: plain(returned) == plain(text), 'return to identical document scroll position')
        capture('images-return')
        remote('resize-os-window', '--width', '55', '--height', '60', '--unit', 'cells')
        wait(lambda text: max(map(len, plain(text).splitlines())) <= 55, 'narrow terminal dimensions')
        # Earlier paragraphs gain wrapped rows; bring the image section back into view.
        remote('send-key', 'ctrl+d')
        narrow = wait(lambda text: image_frame(text) and max(map(len, plain(text).splitlines())) <= 55
                      and any('image-before' in row and PLACEHOLDER in row for row in text.splitlines()),
                      'image/table narrow reflow')
        clean_ui(narrow)
        capture('images-narrow')

    try:
        kitty.start(screen='1600x1400x24')
        for exit_key in ('q', 'esc', 'ctrl+c'):
            terminal = kitty.launch([binary, str(fixture)], height=60,
                                    log_name='kitty-' + exit_key.replace('+', '-') + '.log')
            if exit_key == 'q':
                initial = wait(layout_ready, 'complete rendered Markdown layout', timeout=150)
                positions = check_layout(initial)
                (root / 'list-positions.json').write_text(json.dumps(positions, indent=2) + '\n')
                capture('layout-wide')
                remote('send-key', 'ctrl+d')
                down = wait(lambda text: 'Markdown layout probe' not in text
                            and 'Scroll paragraph' in text, 'half-page scroll down')
                clean_ui(down)
                capture('scroll-down')
                remote('send-key', 'ctrl+u')
                restored = wait(layout_ready, 'half-page scroll back to layout')
                assert check_layout(restored) == positions, 'list layout changed after scrolling'
                capture('scroll-return')
                remote('resize-os-window', '--width', '55', '--height', '60', '--unit', 'cells')
                # Require the long paragraph to reflow, not a stale wide frame.
                narrow = wait(lambda text: layout_ready(text) and not any(
                    'Fullwidth probe:' in row and 'normally reserved' in row
                    for row in text.splitlines()), 'narrow reflow')
                check_layout(narrow)
                capture('layout-narrow')
                remote('resize-os-window', '--width', '120', '--height', '60', '--unit', 'cells')
                wait(lambda text: layout_ready(text) and any(
                    'Fullwidth probe:' in row and 'normally reserved' in row
                    for row in text.splitlines()), 'wide reflow restored')
                capture('layout-restored')
                check_images()
            else:
                wait(lambda text: 'Markdown layout probe' in text, 'viewer startup')
            remote('send-key', exit_key)
            terminal.wait(timeout=10)
            assert terminal.returncode == 0, f'{exit_key} exit code: {terminal.returncode}'
        for exit_key in ('q', 'esc', 'ctrl+c'):
            terminal = kitty.launch([binary, str(document_dir / 'plot sample.png')], height=60,
                                    log_name='kitty-image-' + exit_key.replace('+', '-') + '.log')
            text = wait(lambda text: 'h/j/k/l pan' in text and 'source 192x96' in text
                        and PLACEHOLDER in text, 'direct image startup')
            clean_ui(text)
            if exit_key == 'q':
                capture('direct-image-fit')
                remote('send-text', '--stdin', data=b'+')
                wait(lambda text: 'Zoom 125%' in text, 'direct image zoom')
                capture('direct-image-zoom')
                remote('send-text', '--stdin', data=b'l')
                time.sleep(0.2)
                capture('direct-image-pan')
            remote('send-key', exit_key)
            terminal.wait(timeout=10)
            assert terminal.returncode == 0, f'direct image {exit_key} exit code: {terminal.returncode}'
        # Config/data isolation makes accidental auth or history setup observable.
        assert not list((root / 'data').rglob('*.sqlite*')), 'viewer created session history'
        print(f'PASS: real Kitty Markdown layout/math/images/preview/scroll/resize/exits and direct image viewing; tmux={args.tmux}: {root}')
    except BaseException:
        (root / 'failure.log').write_text(traceback.format_exc())
        # Save evidence before destroying the PTY, even when an assertion fails.
        try:
            capture('failure-screen')
        except BaseException:
            (root / 'failure-capture.log').write_text(traceback.format_exc())
        raise
    finally:
        kitty.close()


if __name__ == '__main__':
    main()

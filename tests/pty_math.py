#!/usr/bin/env python3
"""Exercise Kitty startup and real MathJax in a PTY, without inference or sockets.

The driver acknowledges direct Kitty queries and checks PNG uploads/placeholders.
Tmux mode supplies detected client metadata and never sends a graphics reply.
It is a protocol test; use kitty_visual.py for actual terminal screenshots.
Prepare the optional dependencies with ./ttc --install-math first.
"""
import argparse
import fcntl
import json
import os
from pathlib import Path
import pty
import re
import select
import signal
import struct
import tempfile
import termios
import time
import traceback

from scratch import private_scratch


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary', default='./ttc')
    parser.add_argument('--tmux', action='store_true', help='Exercise DCS wrapping; no tmux server needed')
    parser.add_argument('--disable-color', nargs='?', const='tcell', choices=('tcell', 'no-color'),
                        help='Check explicit TCELL_TRUECOLOR=disable or NO_COLOR fallback')
    args = parser.parse_args()
    root = Path(tempfile.mkdtemp(prefix='pty-math-', dir=private_scratch()))
    script = root / 'script.json'
    script.write_text(json.dumps([{'text': r'''# Demo formulas

Inline math: $\bar{x} = 4$ with $n = 3$ samples.

$$
\bar{x} = \frac{1}{n}\sum_{i=1}^{n} x_i = 4
$$

Formula fixture done.
'''}]))
    binary = str(Path(args.binary).resolve())
    if args.tmux:
        mock_bin = root / 'bin'
        mock_bin.mkdir(mode=0o700)
        mock_tmux = mock_bin / 'tmux'
        mock_tmux.write_text("#!/bin/sh\nprintf 'kitty(0.49.1)\\non\\nbpaste,RGB,title\\n'\n")
        mock_tmux.chmod(0o700)
    pid, fd = pty.fork()
    if pid == 0:
        os.environ['TERM'] = 'tmux-256color'
        for name in ('COLORTERM', 'TCELL_TRUECOLOR', 'NO_COLOR', 'TMUX', 'KITTY_WINDOW_ID'):
            os.environ.pop(name, None)
        if args.tmux:
            os.environ['TMUX'] = '/protocol-fixture/default,1,0'
            os.environ['TMUX_PANE'] = '%21'
            os.environ['PATH'] = str(mock_bin) + os.pathsep + os.environ['PATH']
        if args.disable_color:
            os.environ['TCELL_TRUECOLOR' if args.disable_color == 'tcell' else 'NO_COLOR'] = (
                'disable' if args.disable_color == 'tcell' else '1')
        os.execv(binary, [binary, '--offline-script', str(script), '--data-dir', str(root / 'data'),
                         '--workdir', str(root), '--auto-name=false'])
    fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack('HHHH', 32, 100, 800, 512))
    output = bytearray()
    answered = submitted = False
    try:
        deadline = time.monotonic() + 20
        while time.monotonic() < deadline:
            if select.select([fd], [], [], 0.05)[0]:
                chunk = os.read(fd, 65536)
                if not chunk:
                    raise AssertionError('TTC exited before rendering')
                output.extend(chunk)
                (root / 'terminal.log').write_bytes(output)
            if not args.tmux and not answered and b'i=31337' in output:
                os.write(fd, b'\x1b_Gi=31337;OK\x1b\\')
                answered = True
            if not submitted and b'/help' in output:
                os.write(fd, b'Render demo\r')
                submitted = True
            normalized = bytes(output).replace(b'\x1b\x1b', b'\x1b')
            uploads = re.findall(rb'\x1b_G([^;]*);', normalized)
            ids = {match.group(1) for header in uploads if b'a=T' in header
                   for match in [re.search(rb'(?:^|,)i=(\d+)(?:,|$)', header)] if match}
            if args.disable_color:
                ready = b'24-bit color is disabled' in output and b'Formula fixture done.' in output
                assert not ids, 'explicit color disable still uploaded images'
            else:
                ready = len(ids) >= 3 and '\U0010eeee'.encode() in output
            if ready:
                break
        else:
            raise AssertionError(f'Formula output unavailable: Kitty query={answered}, uploads={len(ids)}; see terminal.log')
        assert submitted
        if args.tmux:
            assert not answered and b'i=31337' not in output, 'tmux detection requires an APC reply'
        else:
            assert answered
        if args.tmux and not args.disable_color:
            assert b'\x1bPtmux;' in output, 'missing tmux passthrough wrapper'
        os.write(fd, b'\x03')
        deadline = time.monotonic() + 5
        while time.monotonic() < deadline:
            waited, status = os.waitpid(pid, os.WNOHANG)
            if waited:
                pid = 0
                assert os.waitstatus_to_exitcode(status) == 0, status
                break
            time.sleep(0.02)
        else:
            raise AssertionError('TTC did not exit')
        print(f'PASS: Kitty protocol/math PTY, COLORTERM absent, tmux={args.tmux}, color disabled={args.disable_color}: {root}')
    except BaseException:
        (root / 'failure.log').write_text(traceback.format_exc())
        print(f'Failure artifacts: {root}', flush=True)
        raise
    finally:
        if pid:
            os.kill(pid, signal.SIGKILL)
            os.waitpid(pid, 0)
        os.close(fd)


if __name__ == '__main__':
    main()

"""Small real-Kitty/Xvfb harness shared by terminal integration tests."""
import os
import select
import signal
import subprocess
import time


class KittyTerminal:
    """Own an isolated display, Kitty PTY and optional private tmux server.

    Call start inside the test's try/finally and always call close. Artifacts
    remain in root; tests own failure capture before cleanup destroys the PTY.
    """

    def __init__(self, root, tmux=False):
        self.root = root
        self.tmux = tmux
        self.sock = root / 'control.sock'
        self.tmux_sock = root / 'tmux.sock'
        self.tmux_config = root / 'tmux.conf'
        self.tmux_config.write_text('set -g default-terminal tmux-256color\n'
                                    'set -g allow-passthrough on\n'
                                    'set -as terminal-features ",xterm-kitty:RGB"\n')
        self.env = dict(os.environ, LIBGL_ALWAYS_SOFTWARE='1')
        # This Kitty PTY is outside the caller's tmux. Stale identity can
        # disable automatic graphics probing, especially in the private tmux.
        for name in ('TMUX', 'TMUX_PANE', 'TERM_PROGRAM', 'TERM_PROGRAM_VERSION',
                     'KITTY_WINDOW_ID', 'NO_COLOR'):
            self.env.pop(name, None)
        self.process = self.xvfb = None
        self.logs = []

    def start(self, screen='1280x900x24'):
        read_fd, write_fd = os.pipe()
        try:
            log = (self.root / 'xvfb.log').open('wb')
            self.logs.append(log)
            self.xvfb = subprocess.Popen([
                'Xvfb', '-displayfd', str(write_fd), '-screen', '0',
                screen, '-nolisten', 'tcp',
            ], pass_fds=[write_fd], stdout=log, stderr=log)
            os.close(write_fd)
            write_fd = None
            if not select.select([read_fd], [], [], 10)[0]:
                raise RuntimeError('Xvfb did not become ready')
            display = os.read(read_fd, 64).decode().strip()
            if not display.isdigit():
                raise RuntimeError('Xvfb failed; see xvfb.log')
            self.env['DISPLAY'] = ':' + display
        finally:
            os.close(read_fd)
            if write_fd is not None:
                os.close(write_fd)

    def launch(self, command, height=38, log_name='kitty.log'):
        """Launch a viewer or session; a previous Kitty must already be exited."""
        if self.process is not None and self.process.poll() is None:
            raise RuntimeError('previous Kitty is still running')
        self.sock.unlink(missing_ok=True)
        if self.tmux:
            command = ['tmux', '-S', str(self.tmux_sock), '-f', str(self.tmux_config),
                       'new-session', 'env', '-u', 'KITTY_WINDOW_ID',
                       'COLORTERM=truecolor', *command]
        log = (self.root / log_name).open('wb')
        self.logs.append(log)
        self.process = subprocess.Popen([
            'kitty', '--config', 'NONE', '-o', 'linux_display_server=x11',
            '-o', 'allow_remote_control=socket-only', '-o', 'enable_audio_bell=no',
            '-o', 'remember_window_size=no', '-o', 'initial_window_width=120c',
            '-o', f'initial_window_height={height}c', '-o', 'font_size=11',
            '--listen-on', f'unix:{self.sock}', *command,
        ], env=self.env, stdout=log, stderr=log, start_new_session=True)
        deadline = time.monotonic() + 15
        while not self.sock.exists():
            if self.process.poll() is not None or time.monotonic() > deadline:
                raise RuntimeError('Kitty did not create its control socket')
            time.sleep(0.05)
        return self.process

    def remote(self, command, *arguments, data=None):
        result = subprocess.run(['kitten', '@', '--to', f'unix:{self.sock}', command, *arguments],
                                env=self.env, input=data, capture_output=True, timeout=10)
        with (self.root / 'remote.log').open('ab') as log:
            log.write((command + ' ' + repr(arguments) + '\n').encode())
            log.write(result.stdout + result.stderr)
        result.check_returncode()
        return result.stdout.decode()

    def capture(self, name):
        self.remote('screenshot', str(self.root / (name + '.png')))
        (self.root / (name + '.txt')).write_text(self.remote('get-text', '--extent', 'screen'))

    def close(self):
        if self.process is not None and self.process.poll() is None:
            os.killpg(self.process.pid, signal.SIGTERM)
            try:
                self.process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                os.killpg(self.process.pid, signal.SIGKILL)
                self.process.wait()
        if self.tmux and self.tmux_sock.exists():
            result = subprocess.run(['tmux', '-S', str(self.tmux_sock), 'kill-server'],
                                    capture_output=True, timeout=5)
            (self.root / 'tmux-cleanup.log').write_bytes(result.stdout + result.stderr)
        if self.xvfb is not None:
            self.xvfb.terminate()
            try:
                self.xvfb.wait(timeout=5)
            except subprocess.TimeoutExpired:
                self.xvfb.kill()
                self.xvfb.wait()
        for log in self.logs:
            log.close()

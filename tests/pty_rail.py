#!/usr/bin/env python3
"""Real Bubblewrap/tmux regression; no credentials or Internet access required."""
import argparse
import fcntl
import hashlib
import json
import os
from pathlib import Path
import pty
import select
import shlex
import shutil
import signal
import socket
import struct
import subprocess
import termios
import threading
import time
import traceback
import tempfile

from scratch import private_scratch


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary', default='./ttc', help='TTC executable to test')
    binary = str(Path(parser.parse_args().binary).resolve())
    root = Path(tempfile.mkdtemp(prefix='rail-', dir=private_scratch()))
    home, project, runtime = root / 'home', root / "project 'quoted'", root / 'r'
    for path in (home, project, runtime, home / '.config', home / '.config/ttc'):
        path.mkdir(mode=0o700)
    global_config = home / '.config/ttc/rail.json'
    local_config = project / 'ttc-rail.json'
    outside = root / 'readonly.txt'
    outside.write_text('outside sentinel')
    (project / 'secret').mkdir()
    (project / 'secret/value').write_text('not visible')
    (home / '.bashrc').write_text('PS1="rail-test$ "\n')
    (home / '.zshrc').write_text('# read-only fixture\n')
    (home / '.tmux.conf').write_text('set -g history-limit 2000\n')
    (home / '.gitconfig').write_text('[user]\n\tname = Rail Fixture\n')
    (home / '.config/nvim').mkdir()
    (home / '.config/nvim/init.lua').write_text('-- read-only fixture\n')
    (home / '.zsh_history').write_text('history fixture\n')
    # Mount a complete fixture rather than relying on host SSH configuration:
    # nested rail maps host root ownership to nobody, which is not trusted.
    ssh_config = root / 'ssh-config'
    ssh_vendor = root / 'ssh-vendor'
    ssh_vendor_dest = Path('/ttc-rail-ssh-fixture')
    ssh_dropins = ssh_config / 'ssh_config.d'
    ssh_dropins.mkdir(parents=True)
    ssh_vendor.mkdir()
    (ssh_config / 'ssh_config').write_text(
        'Include /etc/ssh/ssh_config.d/*.conf\nHost *\n    BatchMode yes\n')
    ssh_source = ssh_vendor / 'vendor.conf'
    ssh_source.write_text('Host *\n    Port 17022\n    ServerAliveInterval 37\n')
    ssh_link = Path('/etc/ssh/ssh_config.d/10-rail-vendor.conf')
    ssh_target = ssh_vendor_dest / 'vendor.conf'
    (ssh_dropins / ssh_link.name).symlink_to(ssh_target)
    ssh_direct = Path('/etc/ssh/ssh_config.d/20-rail-direct.conf')
    (ssh_dropins / ssh_direct.name).write_text('Host *\n    ConnectTimeout 19\n')
    for path in (ssh_config / 'ssh_config', ssh_source, ssh_dropins / ssh_direct.name):
        path.chmod(0o644)
    ssh_mounts = [{'source': str(ssh_config), 'dest': '/etc/ssh', 'mode': 'ro'},
                  {'source': str(ssh_vendor), 'dest': str(ssh_vendor_dest), 'mode': 'ro'}]

    def write_global_config(deny):
        # Every recreation must retain the self-contained SSH fixture mounts.
        global_config.write_text(json.dumps({'authorize_services': ['ssh-agent'],
                                            'allow': [str(outside), 'ssh-agent', *ssh_mounts],
                                            'deny': deny}))

    agent_path = root / 'agent.sock'
    agent = socket.socket(socket.AF_UNIX)
    agent.bind(str(agent_path))
    agent.listen()
    agent.settimeout(15)
    write_global_config([])
    local_config.write_text(json.dumps({'deny': ['secret']}))
    env = dict(os.environ, HOME=str(home), XDG_CONFIG_HOME=str(home / '.config'),
               XDG_CACHE_HOME=str(home / '.cache'), XDG_DATA_HOME=str(home / '.local/share'),
               XDG_RUNTIME_DIR=str(runtime), SHELL='/bin/bash', TERM='xterm-256color',
               SSH_AUTH_SOCK=str(agent_path))
    for key in ('TMUX', 'TMUX_PANE', 'ZDOTDIR', 'DOCKER_HOST', 'HISTFILE', 'TTC_DATA_DIR'):
        env.pop(key, None)
    for key in list(env):
        if key.startswith('GIT_CONFIG'):
            env.pop(key)
    clients, sockets = [], []
    logs = {}

    def run(*args, check=True):
        result = subprocess.run(args, env=env, text=True, capture_output=True, timeout=15)
        if check and result.returncode:
            raise AssertionError(f'{args}: {result.returncode}\n{result.stdout}\n{result.stderr}')
        return result

    def socket_for(workdir):
        key = hashlib.sha256(str(workdir.resolve()).encode()).hexdigest()[:24]
        return runtime / 'ttc/rail' / key / 'run/server.sock'

    def tmux(sock, *args):
        return run('tmux', '-S', str(sock), *args).stdout

    def drain(client, seconds=0.05):
        end = time.monotonic() + seconds
        while time.monotonic() < end:
            if select.select([client['fd']], [], [], min(0.02, max(0, end-time.monotonic())))[0]:
                try:
                    chunk = os.read(client['fd'], 65536)
                except OSError:
                    break
                if not chunk:
                    break
                client['output'].extend(chunk)
        (root / client['log']).write_bytes(client['output'])

    def start(workdir):
        fd, slave = pty.openpty()
        fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack('HHHH', 30, 110, 0, 0))
        proc = subprocess.Popen([binary, 'rail', '--workdir', str(workdir)], env=env,
                                stdin=slave, stdout=slave, stderr=slave, start_new_session=True)
        os.close(slave)
        client = {'proc': proc, 'fd': fd, 'output': bytearray(), 'log': f'client-{len(clients)}.log',
                  'workdir': str(workdir.resolve())}
        clients.append(client)
        return client

    def wait_attach(client, sock):
        deadline = time.monotonic() + 15
        while time.monotonic() < deadline:
            drain(client)
            if client['proc'].poll() is not None:
                raise AssertionError(f"rail client exited {client['proc'].returncode}: {client['output']!r}")
            # Do not connect a native client before rail declares readiness:
            # that would hide a broken bootstrap of foreground tmux -D.
            listing = run(binary, 'rail', '--list', check=False)
            if listing.returncode == 0 and client['workdir'] in listing.stdout:
                result = run('tmux', '-S', str(sock), 'list-clients', '-F', '#{client_pid}', check=False)
                if result.returncode == 0 and result.stdout.strip():
                    return
        raise AssertionError(f"attach timeout: {client['output']!r}")

    def detach(client, sock):
        tmux(sock, 'detach-client', '-a')
        tmux(sock, 'detach-client')
        for candidate in clients:
            if candidate['proc'].poll() is None:
                drain(candidate, 0.1)
                candidate['proc'].wait(timeout=5)
        assert client['proc'].returncode == 0

    def wait_file(path):
        deadline = time.monotonic() + 10
        while time.monotonic() < deadline:
            if path.exists():
                return
            time.sleep(0.03)
        raise AssertionError(f'missing {path}')

    def wait_stopped(sock):
        deadline = time.monotonic() + 5
        while time.monotonic() < deadline:
            if not sock.exists():
                return
            time.sleep(0.03)
        raise AssertionError(f'instance not cleaned up: {sock}')

    ssh_probe = project / 'ssh-probe.py'
    ssh_probe.write_text('''import json, os, subprocess, sys
from pathlib import Path
link, target, direct, output = map(Path, sys.argv[1:])
result = {'symlink': link.is_symlink(), 'link': os.readlink(link),
          'canonical': str(link.resolve()), 'readonly': {}, 'owners': {}}
for path in (link, target, direct):
    result['owners'][str(path)] = path.stat().st_uid
    try:
        with path.open('a'):
            pass
        result['readonly'][str(path)] = False
    except OSError:
        result['readonly'][str(path)] = True
result['target-text'] = target.read_text()
result['direct-text'] = direct.read_text()
# -G evaluates the default system config without connecting or using keys.
ssh = subprocess.run(['ssh', '-G', 'rail-snapshot.invalid'],
                     stdin=subprocess.DEVNULL, capture_output=True, text=True, timeout=5)
result.update({'status': ssh.returncode, 'stdout': ssh.stdout, 'stderr': ssh.stderr})
output.write_text(json.dumps(result))
''')

    def check_ssh(sock, label, port, direct_denied=False):
        output = project / f'ssh-{label}.json'
        done = project / f'ssh-{label}.done'
        command = ' '.join(shlex.quote(str(arg)) for arg in
                           ('python3', ssh_probe, ssh_link, ssh_target, ssh_direct, output))
        tmux(sock, 'new-window', '-d', '-n', f'ssh-{label}', '-c', str(project),
             command + ' > ' + shlex.quote(str(project / f'ssh-{label}.log')) +
             ' 2>&1; touch ' + shlex.quote(str(done)))
        wait_file(done)
        assert output.exists(), (project / f'ssh-{label}.log').read_text()
        result = json.loads(output.read_text())
        assert result['status'] == 0, result
        values = dict(line.split(' ', 1) for line in result['stdout'].splitlines())
        assert values['port'] == str(port) and values['serveraliveinterval'] == '37', result
        assert values['batchmode'] == 'yes', result
        assert values['connecttimeout'] == ('none' if direct_denied else '19'), result
        assert result['symlink'] and result['link'] == str(ssh_target), result
        assert result['canonical'] == str(ssh_target), result
        assert all(result['readonly'].values()), result
        for path in (ssh_link, ssh_target) if direct_denied else (ssh_link, ssh_target, ssh_direct):
            assert result['owners'][str(path)] == os.getuid(), result
        assert result['target-text'] == f'Host *\n    Port {port}\n    ServerAliveInterval 37\n', result
        assert result['direct-text'] == ('' if direct_denied else 'Host *\n    ConnectTimeout 19\n'), result

    listener = socket.socket()
    listener.bind(('127.0.0.1', 0))
    listener.listen()
    listener.settimeout(15)

    def serve_network():
        try:
            conn, _ = listener.accept()
            with conn:
                conn.sendall(b'host-network-ok')
        except OSError:
            pass

    thread = threading.Thread(target=serve_network, daemon=True)
    thread.start()

    def serve_agent():
        try:
            conn, _ = agent.accept()
            with conn:
                conn.sendall(b'forwarded-agent-ok')
        except OSError:
            pass

    agent_thread = threading.Thread(target=serve_agent, daemon=True)
    agent_thread.start()
    try:
        assert run(binary, 'rail', '--list').stdout == 'No live rail sessions.\n'
        sock = socket_for(project)
        sockets.append(sock)
        first = start(project)
        wait_attach(first, sock)
        fcntl.ioctl(first['fd'], termios.TIOCSWINSZ, struct.pack('HHHH', 28, 81, 0, 0))
        os.kill(first['proc'].pid, signal.SIGWINCH)
        deadline = time.monotonic() + 5
        while tmux(sock, 'display-message', '-p', '#{window_width}').strip() != '81' and time.monotonic() < deadline:
            drain(first)
        assert tmux(sock, 'display-message', '-p', '#{window_width}').strip() == '81'
        assert tmux(sock, 'display-message', '-p', '#H').strip() == socket.gethostname() + '-ttc'
        assert tmux(sock, 'display-message', '-p', '#{pane_current_path}').strip() == str(project)
        assert tmux(sock, 'show-options', '-gv', 'history-limit').strip() == '2000'
        listing = run(binary, 'rail', '--list').stdout
        assert str(project) in listing and 'rail' in listing, listing
        original_pid = tmux(sock, 'display-message', '-p', '#{pid}').strip()
        check_ssh(sock, 'initial', 17022)

        # tmux detection must list without needing a terminal, valid workdir,
        # or storage default, and must not create another instance/client.
        nested_env = dict(env, TMUX=f'{sock},123,0', TTC_DATA_DIR='relative')
        for args in ([], ['--workdir', str(root / 'missing')], ['--list']):
            nested = subprocess.run([binary, 'rail', *args], env=nested_env,
                                    text=True, capture_output=True, timeout=15)
            assert nested.returncode == 0, nested
            assert nested.stdout == listing, nested.stdout
            assert ('already inside tmux' in nested.stderr) == ('--list' not in args), nested.stderr
            if '--list' in args:
                assert nested.stderr == '', nested.stderr
        assert tmux(sock, 'display-message', '-p', '#{pid}').strip() == original_pid
        assert len(tmux(sock, 'list-clients').splitlines()) == 1

        probe = project / 'probe.py'
        probe.write_text('''import json, os, socket
from pathlib import Path
result = {}
for path in json.loads(os.environ['RAIL_PROTECTED']):
    try:
        with open(path, 'a') as f: f.write('BAD')
        result[path] = 'WRITABLE'
    except OSError:
        result[path] = 'readonly'
result['secret'] = list(Path('secret').iterdir()) == []
Path('workspace-write').write_text('ok')
for path in json.loads(os.environ['RAIL_STATE']):
    Path(path, 'rail-probe').write_text('ok')
result['private-home'] = not Path.home().joinpath('unimported').exists()
result['uid'] = os.getuid()
result['agent-path'] = os.environ.get('SSH_AUTH_SOCK')
with socket.socket(socket.AF_UNIX) as s:
    s.connect(os.environ['SSH_AUTH_SOCK'])
    result['agent'] = s.recv(100).decode()
with Path.home().joinpath('.zsh_history').open('a') as f:
    f.write('rail history append\\n')
with socket.create_connection(('127.0.0.1', int(os.environ['RAIL_PORT'])), timeout=5) as s:
    result['network'] = s.recv(100).decode()
Path('probe.json').write_text(json.dumps(result))
''')
        (home / 'unimported').write_text('not mounted')
        state = [str(home / '.local/share/ttc'), str(home / '.cache/ttc')]
        protected = [str(global_config), str(local_config), str(outside), str(home / '.bashrc'), str(home / '.zshrc'), str(home / '.tmux.conf'), str(home / '.gitconfig'), str(home / '.config/nvim/init.lua')]
        probe_cmd = 'env ' + ' '.join(shlex.quote(k + '=' + v) for k, v in {
            'RAIL_PROTECTED': json.dumps(protected), 'RAIL_STATE': json.dumps(state),
            'RAIL_PORT': str(listener.getsockname()[1]),
        }.items()) + ' python3 ' + shlex.quote(str(probe))
        tmux(sock, 'new-window', '-d', '-n', 'probe', '-c', str(project), probe_cmd)
        wait_file(project / 'probe.json')
        result = json.loads((project / 'probe.json').read_text())
        assert all(result[path] == 'readonly' for path in protected), result
        assert result['secret'] and result['private-home'], result
        assert result['uid'] == os.getuid() and result['network'] == 'host-network-ok', result
        assert result['agent-path'] == '/run/ssh-agent.sock' and result['agent'] == 'forwarded-agent-ok', result
        assert (home / '.zsh_history').read_text() == 'history fixture\nrail history append\n'
        assert outside.read_text() == 'outside sentinel'
        assert (project / 'workspace-write').read_text() == 'ok'
        if shutil.which('git'):
            tmux(sock, 'new-window', '-d', '-n', 'git-config', '-c', str(project),
                 "git config --global user.name > git-config-name; touch git-config.done")
            wait_file(project / 'git-config.done')
            assert (project / 'git-config-name').read_text().strip() == 'Rail Fixture'
        assert all(Path(path, 'rail-probe').read_text() == 'ok' for path in state)
        if shutil.which('zsh'):
            # File binds cannot be atomically replaced. Exercise the documented
            # in-place save setting with real Zsh, not just a Python append.
            zsh_cmd = "zsh -if -c " + shlex.quote(
                'unsetopt HIST_SAVE_BY_COPY; HISTFILE=$HOME/.zsh_history; '
                'SAVEHIST=100; fc -R; print -s -- rail-zsh-save; fc -W')
            tmux(sock, 'new-window', '-d', '-n', 'history', '-c', str(project),
                 zsh_cmd + ' > zsh-save.log 2>&1; touch zsh-save.done')
            wait_file(project / 'zsh-save.done')
            assert (project / 'zsh-save.log').read_text() == ''
            assert 'rail-zsh-save' in (home / '.zsh_history').read_text()
        # TTC itself is available in the sandbox and uses the shared data mount.
        (project / 'script.json').write_text('[]')
        app_cmd = 'printf "/quit\\n" | ' + shlex.quote(binary) + ' --plain --offline-script script.json > app.log 2>&1; touch app.done'
        tmux(sock, 'new-window', '-d', '-n', 'ttc', '-c', str(project), app_cmd)
        wait_file(project / 'app.done')
        assert 'TTC · Linux terminal agent' in (project / 'app.log').read_text()
        assert (home / '.local/share/ttc/history.sqlite').exists()
        detach(first, sock)
        assert 'rail' in run(binary, 'rail', '--list').stdout

        # A launch-time policy change must not affect an existing instance.
        local_config.write_text('{invalid json')
        # In-place edits are visible through the explicit vendor directory bind,
        # but the canonical drop-in target must remain a launch-time snapshot.
        ssh_source.write_text('Host *\n    Port 17023\n    ServerAliveInterval 37\n')
        check_ssh(sock, 'source-changed', 17022)
        alias = root / 'alias'
        alias.symlink_to(project)
        second, third = start(alias), start(project)
        wait_attach(second, sock)
        wait_attach(third, sock)
        deadline = time.monotonic() + 5
        while len(tmux(sock, 'list-clients').splitlines()) < 2 and time.monotonic() < deadline:
            drain(second)
            drain(third)
        assert len(tmux(sock, 'list-clients').splitlines()) == 2
        assert tmux(sock, 'display-message', '-p', '#{pid}').strip() == original_pid
        check_ssh(sock, 'reattached', 17022)
        assert run(binary, 'rail', '--list').stdout.count(str(project)) == 1
        # Host attachment environment must never replace the instance socket.
        assert tmux(sock, 'show-environment', 'SSH_AUTH_SOCK').strip() == 'SSH_AUTH_SOCK=/run/ssh-agent.sock'
        # detach-client -E replaces the client with a shell command. That command
        # must still execute inside rail, not in the host-side launcher.
        replacement = project / 'detach.py'
        replacement.write_text('''import json, os
from pathlib import Path
result = {'data': os.environ.get('TTC_DATA_DIR')}
try:
    Path(os.environ['RAIL_OUTSIDE']).write_text('HOST ESCAPE')
    result['readonly'] = False
except OSError:
    result['readonly'] = True
Path.home().joinpath('unimported').write_text('private replacement')
Path(os.environ['RAIL_RESULT']).write_text(json.dumps(result))
''')
        command = 'env ' + shlex.quote('RAIL_OUTSIDE=' + str(outside)) + ' ' + shlex.quote('RAIL_RESULT=' + str(project / 'detach-result.json')) + ' python3 ' + shlex.quote(str(replacement))
        for target in tmux(sock, 'list-clients', '-F', '#{client_name}').splitlines():
            tmux(sock, 'detach-client', '-t', target, '-E', command)
        wait_file(project / 'detach-result.json')
        for client in (second, third):
            drain(client, 0.1)
            assert client['proc'].wait(timeout=5) == 0
        detached = json.loads((project / 'detach-result.json').read_text())
        assert detached == {'data': state[0], 'readonly': True}, detached
        assert outside.read_text() == 'outside sentinel'
        assert (home / 'unimported').read_text() == 'not mounted'
        tmux(sock, 'kill-session', '-t', 'rail')
        wait_stopped(sock)
        assert run(binary, 'rail', '--list').stdout == 'No live rail sessions.\n'

        # Bad new config fails closed and includes actionable diagnostics.
        failed = start(project)
        deadline = time.monotonic() + 10
        while failed['proc'].poll() is None and time.monotonic() < deadline:
            drain(failed)
        assert failed['proc'].wait(timeout=2) != 0
        drain(failed)
        assert b'rail config' in failed['output'], failed['output']
        assert run(binary, 'rail', '--list').stdout == 'No live rail sessions.\n'

        # Concurrent creation converges on a single server.
        local_config.write_text('{}')
        write_global_config(['ssh-agent', str(home / '.config/nvim'), str(ssh_direct)])
        fourth, fifth = start(project), start(alias)
        wait_attach(fourth, sock)
        wait_attach(fifth, sock)
        assert run(binary, 'rail', '--list').stdout.count(str(project)) == 1
        # Recreation refreshes the vendor snapshot; an explicit drop-in deny
        # still wins over the generated direct-file snapshot overlay.
        check_ssh(sock, 'recreated', 17023, direct_denied=True)
        denied_probe = "python3 -c " + shlex.quote(
            "import json, os; from pathlib import Path; "
            "Path('denied.json').write_text(json.dumps({"
            "'socket': os.environ.get('SSH_AUTH_SOCK'), "
            "'nvim': list(str(p) for p in Path.home().joinpath('.config/nvim').iterdir())}))")
        tmux(sock, 'new-window', '-d', '-n', 'denied', '-c', str(project), denied_probe)
        wait_file(project / 'denied.json')
        assert json.loads((project / 'denied.json').read_text()) == {'socket': None, 'nvim': []}
        detach(fourth, sock)
        interrupted = start(project)
        wait_attach(interrupted, sock)
        os.kill(interrupted['proc'].pid, signal.SIGTERM)
        drain(interrupted, 0.2)
        assert interrupted['proc'].wait(timeout=5) != 0
        assert 'rail' in run(binary, 'rail', '--list').stdout
        tmux(sock, 'kill-session', '-t', 'rail')
        wait_stopped(sock)
        print(f'rail PTY/sandbox regression passed: {root}')
    except BaseException:
        (root / 'failure.log').write_text(traceback.format_exc())
        for path in (runtime / 'ttc/rail').glob('*/server.log'):
            logs[str(path)] = path.read_text(errors='replace')
        (root / 'server-logs.json').write_text(json.dumps(logs, indent=2))
        print(f'rail failure evidence: {root}')
        raise
    finally:
        listener.close()
        agent.close()
        for sock in sockets:
            run('tmux', '-S', str(sock), 'kill-server', check=False)
        for client in clients:
            if client['proc'].poll() is None:
                client['proc'].terminate()
                client['proc'].wait(timeout=5)
            drain(client)
            os.close(client['fd'])


if __name__ == '__main__':
    main()

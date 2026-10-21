#!/usr/bin/env python3
"""Socket-free PTY regression for main/child event-driven awaiting and cleanup."""
import argparse
import errno
import json
import os
from pathlib import Path
import pty
import select
import signal
import sqlite3
import subprocess
import tempfile
import time
import traceback

from scratch import private_scratch


def call(identifier, tool_name, **arguments):
    return {'id': identifier, 'name': tool_name, 'arguments': arguments}


class Instance:
    def __init__(self, root, binary, name, responses):
        self.root = root / name
        self.root.mkdir()
        self.project = self.root / 'project'
        self.project.mkdir()
        self.database = self.root / 'data/history.sqlite'
        script = self.root / 'script.json'
        script.write_text(json.dumps(responses, indent=2))
        self.output = bytearray()
        self.cursor = 0
        self.master, slave = pty.openpty()
        try:
            self.process = subprocess.Popen([
                binary, '--plain', '--auto-name=false', '--workdir', str(self.project),
                '--data-dir', str(self.root / 'data'), '--offline-script', str(script),
            ], stdin=slave, stdout=slave, stderr=slave, start_new_session=True)
        finally:
            os.close(slave)

    def send(self, text):
        os.write(self.master, (text + '\n').encode())

    def read(self, timeout=0.05):
        if select.select([self.master], [], [], timeout)[0]:
            try:
                data = os.read(self.master, 65536)
            except OSError as error:
                if error.errno != errno.EIO:
                    raise
                data = b''
            self.output.extend(data)
            (self.root / 'terminal.log').write_bytes(self.output)
            if not data and self.process.poll() is not None:
                raise AssertionError(f'PTY exited ({self.process.returncode}): {self.root}')

    def until(self, predicate, description, timeout=8):
        deadline = time.monotonic() + timeout
        while not predicate():
            assert time.monotonic() < deadline, f'timed out: {description}; {self.root}'
            self.read()

    def expect(self, text):
        needle = text.encode()
        self.until(lambda: self.output.find(needle, self.cursor) >= 0, repr(text))
        self.cursor = self.output.find(needle, self.cursor) + len(needle)

    def rows(self, sql, parameters=()):
        with sqlite3.connect(self.database) as db:
            return db.execute(sql, parameters).fetchall()

    def delivered(self, actor):
        requests = self.rows("SELECT id,delivered_events_json FROM model_requests WHERE actor_id=? AND purpose='coding' ORDER BY id", (actor,))
        result = []
        for request, events in requests:
            contents = []
            for event in json.loads(events):
                contents.extend(row[0] for row in self.rows('SELECT content_json FROM entries WHERE id=?', (event,)))
            contents.extend(row[0] for row in self.rows('SELECT content_json FROM entries WHERE delivered_request_id=?', (request,)))
            result.append('\n'.join(contents))
        return result

    def assert_waiting(self, actor='main', requests=1):
        # The explicit frontend event, not elapsed time, establishes the wait.
        self.expect('Awaiting events')
        assert b'Turn complete' not in self.output, self.root
        assert self.rows("SELECT count(*) FROM turns WHERE actor_id='main' AND status='completed'") == [(0,)]
        assert self.rows("SELECT count(*) FROM model_requests WHERE actor_id=? AND purpose='coding'", (actor,)) == [(requests,)]
        assert self.rows('SELECT count(*) FROM tool_calls WHERE result_json IS NULL') == [(0,)]
        # No final assistant message is allowed to settle an awaiting assignment.
        assert not self.rows("SELECT id FROM entries WHERE actor_id=? AND role='assistant' AND json_extract(content_json,'$.content')!=''", (actor,))

    def finish(self, text):
        self.expect('Resuming')
        self.expect(text)
        self.expect('Turn complete')
        self.until(lambda: self.rows("SELECT count(*) FROM turns WHERE actor_id='main' AND trigger!='steer' AND status='completed'") == [(1,)], 'durable turn completion')
        failed = self.rows("SELECT id,status,attempts_json FROM model_requests WHERE status!='completed'")
        assert not failed, failed
        assert b'Error:' not in self.output, self.root

    def dump(self):
        if self.database.exists():
            with sqlite3.connect(self.database) as db:
                for table in ('turns', 'model_requests', 'tool_calls', 'entries'):
                    cursor = db.execute('SELECT * FROM ' + table)
                    columns = [column[0] for column in cursor.description]
                    (self.root / (table + '.json')).write_text(json.dumps([
                        dict(zip(columns, row)) for row in cursor.fetchall()], indent=2))

    def stop(self, success=False):
        try:
            self.dump()
            if self.process.poll() is None:
                if success:
                    self.send('/quit')
                    self.process.wait(timeout=3)
                    assert self.process.returncode == 0
                else:
                    os.killpg(self.process.pid, signal.SIGTERM)
                    self.process.wait(timeout=3)
        finally:
            if self.process.poll() is None:
                os.killpg(self.process.pid, signal.SIGKILL)
                self.process.wait(timeout=3)
            os.close(self.master)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary', default='./ttc', help='TTC binary, including a scratch build')
    args = parser.parse_args()
    binary = str(Path(args.binary).resolve())
    root = Path(tempfile.mkdtemp(prefix='pty-await-', dir=private_scratch()))
    print(f'Artifacts: {root}', flush=True)
    instance = None
    try:
        # Await deadlines exceed expect's eight-second bound: a missing event
        # signal must fail, not accidentally pass after a deadline resume.
        instance = Instance(root, binary, 'main-timer', [
            {'prefix': 'user: Wait for the timer', 'calls': [
                call('main-timer', 'wakeup_schedule', name='main-wake', message='MAIN-TIMER-EVENT', delay_seconds=3),
                call('main-await', 'await', deadline_seconds=20)]},
            {'text': 'MAIN-TIMER-FINISHED'},
        ])
        instance.send('Wait for the timer')
        instance.assert_waiting()
        instance.finish('MAIN-TIMER-FINISHED')
        requests = instance.delivered('main')
        assert len(requests) == 2, requests
        assert 'MAIN-TIMER-EVENT' in requests[1]
        assert instance.rows("SELECT count(*) FROM turns WHERE actor_id='main'") == [(1,)]
        instance.stop(success=True)
        instance = None

        instance = Instance(root, binary, 'main-input', [
            {'prefix': 'user: Wait for my input', 'calls': [call('input-await', 'await', deadline_seconds=20)]},
            {'prefix': 'user: USER-INPUT-WHILE-AWAITING', 'text': 'MAIN-INPUT-FINISHED'},
        ])
        instance.send('Wait for my input')
        instance.assert_waiting()
        instance.send('USER-INPUT-WHILE-AWAITING')
        instance.finish('MAIN-INPUT-FINISHED')
        requests = instance.delivered('main')
        assert len(requests) == 2, requests
        assert instance.rows("SELECT json_extract(content_json,'$.content') FROM entries WHERE json_extract(content_json,'$.input_source')='steer'") == [('USER-INPUT-WHILE-AWAITING',)]
        assert instance.rows("SELECT trigger,status FROM turns WHERE actor_id='main' ORDER BY started_ms") == [('user', 'completed'), ('steer', 'completed')]
        instance.stop(success=True)
        instance = None

        instance = Instance(root, binary, 'child-timer', [
            {'prefix': 'user: Run the timer child', 'calls': [call(
                'launch-timer-child', 'subagent', prompt='CHILD-TIMER-TASK', label='timer', persistent=False)]},
            {'prefix': 'user: CHILD-TIMER-TASK', 'calls': [
                call('child-wake', 'wakeup_schedule', name='child-wake', message='CHILD-TIMER-EVENT', delay_seconds=3),
                call('child-timer-await', 'await', deadline_seconds=20)]},
            {'text': 'CHILD-TIMER-FINISHED'},
            {'text': 'PARENT-TIMER-FINISHED'},
        ])
        instance.send('Run the timer child')
        instance.expect('Awaiting events')
        assert b'Turn complete' not in instance.output
        assert instance.rows("SELECT count(*) FROM tool_calls WHERE actor_id!='main' AND result_json IS NULL") == [(0,)]
        assert not instance.rows("SELECT id FROM entries WHERE actor_id!='main' AND role='assistant' AND json_extract(content_json,'$.content')!=''")
        instance.finish('PARENT-TIMER-FINISHED')
        actor = instance.rows("SELECT DISTINCT actor_id FROM model_requests WHERE actor_id!='main'")[0][0]
        requests = instance.delivered(actor)
        assert len(requests) == 2 and 'CHILD-TIMER-EVENT' in requests[1], requests
        instance.stop(success=True)
        instance = None

        # Foreground launch fixes scripted-provider request order while the child
        # itself owns asynchronous work. No network, credentials, or timed sleep.
        instance = Instance(root, binary, 'child-shell', [
            {'prefix': 'user: Run the persistent child', 'calls': [call(
                'launch-child', 'subagent', prompt='CHILD-AWAIT-TASK', label='waiter', persistent=True)]},
            {'prefix': 'user: CHILD-AWAIT-TASK', 'calls': [
                call('child-timer', 'wakeup_schedule', name='child-leftover', message='MUST-NOT-FIRE', delay_seconds=60, repeat_seconds=60),
                call('child-shell', 'shell', command='printf ready > started; read release < release.fifo; printf CHILD-SHELL-EVENT', background=True),
                call('child-await', 'await', deadline_seconds=20)]},
            {'text': 'CHILD-AWAIT-FINISHED'},
            {'calls': [call('inspect-cleanup', 'wakeup_list', details=True)]},
            {'text': 'PARENT-CHILD-FINISHED'},
        ])
        os.mkfifo(instance.project / 'release.fifo')
        instance.send('Run the persistent child')
        instance.expect('Awaiting events')
        child = instance.rows("SELECT DISTINCT actor_id FROM model_requests WHERE actor_id!='main'")
        assert len(child) == 1, child
        actor = child[0][0]
        assert b'Turn complete' not in instance.output
        assert instance.rows("SELECT count(*) FROM model_requests WHERE actor_id=?", (actor,)) == [(1,)]
        assert instance.rows("SELECT count(*) FROM tool_calls WHERE actor_id=? AND result_json IS NULL", (actor,)) == [(0,)]
        assert not instance.rows("SELECT id FROM entries WHERE actor_id=? AND role='assistant' AND json_extract(content_json,'$.content')!=''", (actor,))
        instance.until(lambda: (instance.project / 'started').exists(), 'background shell started')

        def release_shell():
            try:
                fd = os.open(instance.project / 'release.fifo', os.O_WRONLY | os.O_NONBLOCK)
            except OSError as error:
                if error.errno == errno.ENXIO:
                    return False
                raise
            with os.fdopen(fd, 'w') as stream:
                stream.write('release\n')
            return True

        instance.until(release_shell, 'FIFO reader ready')
        instance.finish('PARENT-CHILD-FINISHED')
        requests = instance.delivered(actor)
        assert len(requests) == 2, requests
        assert 'job_exit' in requests[1], requests[1]
        result = instance.rows("SELECT result_json FROM tool_calls WHERE provider_call_id='inspect-cleanup'")
        timers = json.loads(result[0][0])['wakeups']
        leftover = next(timer for timer in timers if timer['name'] == 'child-leftover')
        assert leftover['status'] == 'cancelled' and leftover['fired_count'] == 0, leftover
        finishes = instance.rows("SELECT content_json FROM entries WHERE json_extract(content_json,'$.type')='child_turn_finished'")
        assert len(finishes) == 1, finishes
        finish = json.loads(finishes[0][0])
        assert finish['persistent'] is True and finish['context_state'] == 'idle', finish
        instance.stop(success=True)
        instance = None
        print('PASS: main timer/input await, child timer/shell wakeups, persistent child timer cleanup')
    except BaseException:
        (root / 'failure.txt').write_text(traceback.format_exc())
        raise
    finally:
        if instance is not None:
            instance.stop()


if __name__ == '__main__':
    main()

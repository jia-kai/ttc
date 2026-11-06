"""Offline launcher checks: fake Podman/Git/Go, synthetic credentials, no inference."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

REV = '452bf305c6daa62fc59061d22133a7cbc7c1572e'
TASKS = ('mvcc-lsm-compaction', 'bun-sourcemap-leak')
# Declared artifacts from the published task packages, not all of /app.
ARTIFACTS = (
    ['Makefile', 'crash_report.txt']
    + ['include/mvcc_lsm/' + name + '.h' for name in (
        'flush_builder', 'lsm_db', 'snapshot_context', 'types', 'version', 'version_store')]
    + ['src/' + name + '.cc' for name in (
        'flush_builder', 'lsm_db', 'snapshot_context', 'version_store')]
    + ['tests/regression_test.cc', 'tests/stress_repro.cc'],
    ['package.json', 'tsconfig.json', 'visibility.json', 'scripts/release.ts', 'src/client.ts'],
)
MOCK = r'''#!/usr/bin/env python3
import json, os, pathlib, shutil, sys
P = pathlib.Path
root = P(os.environ['MOCK_ROOT'])
args = sys.argv[1:]
tool = P(sys.argv[0]).name
with (root / 'calls').open('a') as f:
    f.write(json.dumps([tool] + args) + '\n')
if tool == 'git':
    if args[0] == 'clone': shutil.copytree(root / 'fixture', args[-1])
    elif 'rev-parse' in args: print(os.environ.get('REVISION', '452bf305c6daa62fc59061d22133a7cbc7c1572e'))
    elif 'status' in args: print(os.environ.get('DIRTY', ''), end='')
elif tool == 'make': P('ttc').write_text('mock binary\n')
elif tool == 'go': pass
elif tool == 'podman':
    def container(name): return root / 'containers' / name
    cmd = args[0]
    if cmd == 'info': sys.exit(int(os.environ.get('INFO_EXIT', '0')))
    elif cmd == 'container': sys.exit(0 if container(args[-1]).exists() else 1)
    elif cmd == 'run':
        name = args[args.index('--name') + 1]
        c = container(name)
        (c / 'app').mkdir(parents=True)
        (c / 'meta').write_text(json.dumps({'running': True, 'root': str(P.cwd())}))
        if name.endswith('-verifier'):
            for d in ['include/mvcc_lsm', 'src', 'tests']:
                (c / 'app' / d).mkdir(parents=True, exist_ok=True)
        else:
            task = name.removeprefix('ttc-bench-v4-')
            shutil.copytree(root / 'apps' / task, c / 'app', dirs_exist_ok=True, symlinks=True)
        print(name)
    elif cmd == 'inspect':
        meta = json.loads((container(args[-1]) / 'meta').read_text())
        if 'ttc-bench.root' in args[2]: print(meta['root'])
        elif 'ttc-bench.revision' in args[2]: print('452bf305c6daa62fc59061d22133a7cbc7c1572e')
        else: print(str(meta['running']).lower())
    elif cmd == 'rm': shutil.rmtree(container(args[-1]))
    elif cmd in ['stop', 'start']:
        p = container(args[-1]) / 'meta'
        m = json.loads(p.read_text()); m['running'] = cmd == 'start'
        p.write_text(json.dumps(m))
    elif cmd == 'cp':
        src, dst = args[1:]
        if ':' in src:
            name, p = src.split(':', 1); s = container(name) / p.lstrip('/'); d = P(dst)
        else:
            name, p = dst.split(':', 1); s = P(src); d = container(name) / p.lstrip('/')
        if s.is_symlink():
            d.parent.mkdir(parents=True, exist_ok=True); d.symlink_to(os.readlink(s))
        elif s.is_dir(): shutil.copytree(s, d, dirs_exist_ok=True, symlinks=True)
        else:
            d.parent.mkdir(parents=True, exist_ok=True); shutil.copy2(s, d)
    elif cmd == 'exec':
        ix = next(i for i, a in enumerate(args) if a.startswith('ttc-bench-v4-'))
        c = container(args[ix]); command = args[ix + 1:]
        if command[0] == 'timeout':
            (c / 'logs/verifier').mkdir(parents=True)
            if os.environ.get('NO_REWARD') != '1':
                (c / 'logs/verifier/reward.txt').write_text(os.environ.get('REWARD', '1') + '\n')
            print('mock verifier output')
            sys.exit(int(os.environ.get('VERIFY_EXIT', '0')))
'''


class PrepareTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix='ttc-prepare-')
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.repo = self.root / 'repo with spaces'
        self.repo.mkdir()
        (self.repo / 'go.mod').touch()
        (self.repo / 'cmd/ttc').mkdir(parents=True)
        (self.repo / 'cmd/ttc/main_linux.go').touch()
        shutil.copy2(Path(__file__).resolve().parents[1] / 'prepare.sh', self.repo / 'prepare.sh')
        tools = self.root / 'bin'
        tools.mkdir()
        mock = tools / 'mock'
        mock.write_text(MOCK)
        mock.chmod(0o755)
        for tool in ['git', 'make', 'go', 'podman']:
            (tools / tool).symlink_to(mock)
        for task, artifacts in zip(TASKS, ARTIFACTS):
            source = self.root / 'fixture/tasks' / task
            source.mkdir(parents=True)
            (source / 'instruction.md').write_text('Task instructions, not a solution.\n')
            for artifact in artifacts + ['not-a-declared-artifact.txt']:
                path = self.root / 'apps' / task / artifact
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text('synthetic task file\n')
        self.data = self.root / 'credentials'
        self.data.mkdir()
        (self.data / 'openai-auth.json').write_text('synthetic auth fixture\n')
        self.env = {**os.environ, 'PATH': str(tools) + ':' + os.environ['PATH'],
                    'MOCK_ROOT': str(self.root), 'TTC_DATA_DIR': str(self.data)}

    def run_script(self, *args, code=0, input=None, **env):
        result = subprocess.run(['bash', './prepare.sh', *args], cwd=self.repo,
                                env={**self.env, **env}, input=input,
                                capture_output=True, text=True)
        self.assertEqual(result.returncode, code, result.stdout + result.stderr)
        self.assertNotIn('synthetic auth fixture', result.stdout + result.stderr)
        return result

    def calls(self):
        return [json.loads(line) for line in (self.root / 'calls').read_text().splitlines()]

    def save(self, task=0):
        return self.repo / 'term-bench/v4' / TASKS[task]

    def test_prepare_both_interactive_tasks_and_preserve_old_runs(self):
        old = self.repo / 'term-bench/source/old-run'
        old.parent.mkdir(parents=True)
        old.write_text('keep me')
        for number in ['1', '2']:
            self.run_script(number)
        self.assertEqual(old.read_text(), 'keep me')
        for task in range(2):
            save = self.save(task)
            auth = save / 'state/openai-auth.json'
            self.assertEqual(auth.read_bytes(), (self.data / 'openai-auth.json').read_bytes())
            self.assertEqual(auth.stat().st_mode & 0o777, 0o600)
            self.assertEqual(list((save / 'build').iterdir()), [save / 'build/Containerfile'])
            self.assertIn('@sha256:', (save / 'build/Containerfile').read_text())
        calls = self.calls()
        self.assertFalse(any('/AGENTS.md' in str(c) or '--login' in c for c in calls))
        self.assertFalse(any('/tests/test.sh' in str(c) for c in calls))
        launches = [c for c in calls if 'ttc' in c and '--workdir' in c]
        self.assertEqual(len(launches), 2)
        self.assertTrue(all('-it' in c and '--model' not in c for c in launches))
        builds = [c for c in calls if c[:2] == ['podman', 'build']]
        self.assertTrue(all(c[-1].endswith('/build') for c in builds))

    def test_resume_and_shell_do_not_rebuild(self):
        self.run_script('1')
        self.run_script('1')
        self.run_script('shell', '1')
        self.assertEqual(sum(c[:2] == ['podman', 'build'] for c in self.calls()), 1)

    def test_separate_verifier_only_receives_declared_artifacts(self):
        for index, number in enumerate(['1', '2']):
            self.run_script(number)
            self.run_script('grade', number, input='n\n')
            self.assertFalse((self.save(index) / 'grading-started').exists())
            result = self.run_script('grade', number, input='y\n')
            self.assertIn('PASS:', result.stdout)
            verifier = self.root / 'containers' / ('ttc-bench-v4-' + TASKS[index] + '-verifier')
            actual = {str(p.relative_to(verifier / 'app')) for p in (verifier / 'app').rglob('*') if p.is_file()}
            self.assertEqual(actual, set(ARTIFACTS[index]))
            self.assertTrue((self.save(index) / 'app/not-a-declared-artifact.txt').exists())
            results = next(self.save(index).glob('grade.*'))
            self.assertEqual((results / 'verifier/reward.txt').read_text(), '1\n')
            self.run_script(number, code=1)
        verifiers = [c for c in self.calls() if c[:2] == ['podman', 'run'] and c[c.index('--name') + 1].endswith('-verifier')]
        self.assertEqual(len(verifiers), 2)
        self.assertTrue(all('-v' not in c and '-e' not in c and '@sha256:' in c[-3] for c in verifiers))
        executions = [c for c in self.calls() if 'timeout' in c]
        self.assertEqual([c[c.index('timeout') + 2] for c in executions], ['900', '600'])

    def test_nonzero_test_exit_with_zero_reward_is_a_failure(self):
        self.run_script('2')
        result = self.run_script('grade', '2', input='y\n', code=1, REWARD='0', VERIFY_EXIT='1')
        self.assertIn('FAIL: bun-sourcemap-leak', result.stdout)
        self.assertTrue(next(self.save(1).glob('grade.*/verifier.log')).is_file())

    def test_timeout_is_not_a_scored_failure(self):
        self.run_script('1')
        result = self.run_script('grade', '1', input='y\n', code=1, REWARD='0', VERIFY_EXIT='124')
        self.assertIn('Verifier timed out', result.stderr)
        self.assertNotIn('FAIL:', result.stdout)
        self.assertFalse((self.save() / 'grading-complete').exists())
        # Retry without a new model attempt or replacing the frozen workspace.
        original = (self.save() / 'app/Makefile').read_text()
        agent = self.root / 'containers/ttc-bench-v4-mvcc-lsm-compaction/app/Makefile'
        agent.write_text('changed after snapshot')
        self.run_script('grade', '1', input='y\n')
        self.assertEqual((self.save() / 'app/Makefile').read_text(), original)
        self.assertEqual(len(list(self.save().glob('grade.*'))), 2)
        for result_dir in self.save().glob('grade.*'):
            self.assertTrue((result_dir / 'verifier.log').is_file())

    def test_reject_artifact_and_ancestor_symlinks_before_host_upload(self):
        for number, relative in [('1', 'Makefile'), ('2', 'src')]:
            self.run_script(number)
            task = TASKS[int(number) - 1]
            sentinel = self.root / ('host-only-' + number)
            sentinel.write_text('host-only sentinel, not credentials')
            path = self.root / 'containers' / ('ttc-bench-v4-' + task) / 'app' / relative
            if path.is_dir(): shutil.rmtree(path)
            else: path.unlink()
            path.symlink_to(sentinel)
            result = self.run_script('grade', number, input='y\n', code=1)
            self.assertIn('symlinks or special files', result.stderr)
            self.assertFalse((self.root / 'containers' / ('ttc-bench-v4-' + task + '-verifier')).exists())
            self.assertEqual(sentinel.read_text(), 'host-only sentinel, not credentials')

    def test_reject_unusable_runtime_and_changed_source(self):
        self.run_script('1', code=1, INFO_EXIT='1')
        self.assertFalse((self.repo / 'term-bench').exists())
        self.run_script('1', code=1, REVISION='wrong')
        self.assertFalse(self.save().exists())

    def test_cleanup_archives_graded_run_and_allows_fresh_attempt(self):
        self.run_script('1')
        self.run_script('2')
        (self.save() / 'state/history.sqlite').write_text('synthetic old history')
        self.run_script('grade', '1', input='y\n')
        self.run_script('cleanup', '1')
        self.assertFalse(self.save().exists())
        archives = list(self.save().parent.glob(TASKS[0] + '.saved-*'))
        self.assertEqual(len(archives), 1)
        archive = archives[0]
        self.assertEqual((archive / 'state/history.sqlite').read_text(), 'synthetic old history')
        self.assertTrue((archive / 'state/openai-auth.json').is_file())
        self.assertTrue((archive / 'grading-complete').exists())
        self.assertTrue(next(archive.glob('grade.*/verifier/reward.txt')).is_file())
        for suffix in ['', '-verifier']:
            self.assertFalse((self.root / 'containers' / ('ttc-bench-v4-' + TASKS[0] + suffix)).exists())
        self.assertTrue(self.save(1).exists())
        self.assertTrue((self.root / 'containers' / ('ttc-bench-v4-' + TASKS[1])).exists())
        self.assertTrue((self.repo / 'term-bench/source-v4').is_dir())
        self.run_script('1')
        self.assertFalse((self.save() / 'state/history.sqlite').exists())
        self.assertFalse((self.save() / 'grading-started').exists())
        self.assertEqual(sum(c[:2] == ['podman', 'build'] for c in self.calls()), 3)

    def test_cleanup_preserves_ungraded_workspace_and_is_repeatable(self):
        self.run_script('2')
        agent = self.root / 'containers' / ('ttc-bench-v4-' + TASKS[1])
        (agent / 'app/src/client.ts').write_text('ungraded changes')
        self.run_script('cleanup', '2')
        archive = next(self.save(1).parent.glob(TASKS[1] + '.saved-*'))
        self.assertEqual((archive / 'app/src/client.ts').read_text(), 'ungraded changes')
        self.run_script('cleanup', '2')
        self.assertEqual(list(self.save(1).parent.glob(TASKS[1] + '.saved-*')), [archive])

    def test_cleanup_checks_both_container_owners_before_removal(self):
        self.run_script('1')
        self.run_script('grade', '1', input='y\n')
        agent = self.root / 'containers' / ('ttc-bench-v4-' + TASKS[0])
        verifier = self.root / 'containers' / (agent.name + '-verifier')
        meta = json.loads((verifier / 'meta').read_text())
        meta['root'] = str(self.root / 'another checkout')
        (verifier / 'meta').write_text(json.dumps(meta))
        result = self.run_script('cleanup', '1', code=1)
        self.assertIn('another checkout', result.stderr)
        self.assertTrue(agent.exists())
        self.assertTrue(verifier.exists())
        self.assertTrue(self.save().exists())
        self.assertFalse(list(self.save().parent.glob(TASKS[0] + '.saved-*')))

    def test_cleanup_requires_explicit_task_and_rejects_symlinks(self):
        self.run_script('cleanup', code=1)
        self.assertFalse((self.repo / 'term-bench').exists())
        self.run_script('1')
        original = self.save()
        outside = self.root / 'outside-run'
        original.rename(outside)
        original.symlink_to(outside, target_is_directory=True)
        result = self.run_script('cleanup', '1', code=1)
        self.assertIn('symlink', result.stderr)
        self.assertTrue(outside.is_dir())
        self.assertTrue((self.root / 'containers' / ('ttc-bench-v4-' + TASKS[0])).exists())


if __name__ == '__main__':
    unittest.main()

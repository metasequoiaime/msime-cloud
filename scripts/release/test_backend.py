"""使用临时裸仓库验证版本递增、重复发布与并发推送保护。"""
import contextlib
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import backend


class VersionTests(unittest.TestCase):
    def test_commit_types(self):
        for old, message, expected in [
            ('0.1.0', 'fix: 修复', '0.1.1'),
            ('0.1.0', 'feat(api): 新接口', '0.2.0'),
            ('0.1.0', 'fix!: 不兼容变更', '0.2.0'),
            ('1.2.3', 'fix!: 不兼容变更', '2.0.0'),
            ('1.2.3', 'fix: 修改\nBREAKING CHANGE: 契约变化', '2.0.0'),
            ('1.2.3', 'fix: not a BREAKING CHANGE', '1.2.4'),
        ]:
            with self.subTest(message=message):
                self.assertEqual(backend.next_version(old, message), expected)


class RepositoryTests(unittest.TestCase):
    def setUp(self):
        # git commit and a receiving push start `git maintenance --auto` / `gc --auto` in the background, detached from the command that started them. One still packing objects when the test ends makes the temporary directory's removal fail with "Directory not empty: 'objects'" (seen in CI). Every git process here, the release script's included, inherits this environment.
        environment = patch.dict(os.environ, {
            'GIT_CONFIG_COUNT': '3',
            'GIT_CONFIG_KEY_0': 'gc.auto', 'GIT_CONFIG_VALUE_0': '0',
            'GIT_CONFIG_KEY_1': 'maintenance.auto', 'GIT_CONFIG_VALUE_1': 'false',
            'GIT_CONFIG_KEY_2': 'receive.autogc', 'GIT_CONFIG_VALUE_2': 'false',
        })
        environment.start()
        self.addCleanup(environment.stop)
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.remote = self.root / 'remote.git'
        self.repo = self.root / 'repo'
        self.run_git(self.root, 'init', '--bare', str(self.remote))
        self.run_git(self.root, 'clone', str(self.remote), str(self.repo))
        self.run_git(self.repo, 'checkout', '-b', 'main')
        self.run_git(self.repo, 'config', 'user.name', '测试')
        self.run_git(self.repo, 'config', 'user.email', 'test@example.invalid')
        self.run_git(self.repo, 'config', 'core.autocrlf', 'false')
        (self.repo / 'VERSION').write_text('0.1.0\n')
        (self.repo / 'cmd').mkdir()
        (self.repo / 'cmd/main.go').write_text('package main\n')
        self.commit('feat: 初始后端')
        self.run_git(self.repo, 'push', 'origin', 'main')

    def run_git(self, cwd, *args):
        return subprocess.check_output(['git', *args], cwd=cwd, stderr=subprocess.PIPE, text=True).strip()

    def commit(self, message):
        self.run_git(self.repo, 'add', '.')
        self.run_git(self.repo, 'commit', '-m', message)

    def release(self):
        with contextlib.chdir(self.repo), patch.dict(os.environ, {'GITHUB_OUTPUT': str(self.root / 'outputs')}):
            return backend.release()

    def test_first_and_recovery_release(self):
        first = self.release()
        self.assertEqual(first['version'], '0.1.0')
        self.assertEqual(self.release(), first)
        self.assertEqual(self.run_git(self.remote, 'rev-parse', 'main'), first['revision'])

    def test_docs_skip_then_feature_bump(self):
        first = self.release()
        (self.repo / 'cmd/README.md').write_text('仅文档')
        self.commit('feat: 文档不递增')
        self.run_git(self.repo, 'push', 'origin', 'main')
        self.assertEqual(self.release(), first)
        (self.repo / 'cmd/main.go').write_text('package main\n// 新接口\n')
        self.commit('feat(api): 新接口')
        self.run_git(self.repo, 'push', 'origin', 'main')
        result = self.release()
        self.assertEqual(result['version'], '0.2.0')
        self.assertEqual((self.repo / 'VERSION').read_text(), '0.2.0\n')
        self.assertEqual(self.run_git(self.remote, 'rev-parse', 'main'), result['revision'])

    def test_admin_only_change_triggers_release(self):
        self.release()
        (self.repo / 'admin-web').mkdir()
        (self.repo / 'admin-web/index.html').write_text('<main>Admin</main>')
        self.commit('feat(admin): 管理后台')
        self.run_git(self.repo, 'push', 'origin', 'main')
        self.assertEqual(self.release()['version'], '0.2.0')

    def test_atomic_push_rejects_unvalidated_new_main(self):
        self.release()
        (self.repo / 'cmd/main.go').write_text('package main\n// 修复\n')
        self.commit('fix: 修复')
        self.run_git(self.repo, 'push', 'origin', 'main')
        other = self.root / 'other'
        self.run_git(self.root, 'clone', '--branch', 'main', str(self.remote), str(other))
        self.run_git(other, 'config', 'user.name', '并行提交')
        self.run_git(other, 'config', 'user.email', 'other@example.invalid')
        (other / 'new.txt').write_text('更新的主分支')
        self.run_git(other, 'add', '.')
        self.run_git(other, 'commit', '-m', 'fix: 并行修改')
        self.run_git(other, 'push', 'origin', 'main')
        tip = self.run_git(other, 'rev-parse', 'HEAD')
        with self.assertRaises(subprocess.CalledProcessError):
            self.release()
        self.assertEqual(self.run_git(self.remote, 'rev-parse', 'main'), tip)
        self.assertNotIn('backend-v0.1.1', self.run_git(self.remote, 'tag', '--list'))

    def test_release_yields_to_the_run_a_newer_release_change_queued(self):
        self.release()
        (self.repo / 'cmd/main.go').write_text('package main\n// 修复\n')
        self.commit('fix: 修复')
        self.run_git(self.repo, 'push', 'origin', 'main')
        other = self.root / 'other'
        self.run_git(self.root, 'clone', '--branch', 'main', str(self.remote), str(other))
        self.run_git(other, 'config', 'user.name', '并行提交')
        self.run_git(other, 'config', 'user.email', 'other@example.invalid')
        (other / 'cmd/main.go').write_text('package main\n// 修复\n// 并行修复\n')
        self.run_git(other, 'add', '.')
        self.run_git(other, 'commit', '-m', 'fix: 并行修改')
        self.run_git(other, 'push', 'origin', 'main')
        tip = self.run_git(other, 'rev-parse', 'HEAD')
        outputs = (self.root / 'outputs').read_text()
        # 并行提交改了发布路径，push 已为它排上一次发布；本次不失败，也不给镜像任务输出。
        self.assertIsNone(self.release())
        self.assertEqual((self.root / 'outputs').read_text(), outputs)
        self.assertEqual(self.run_git(self.remote, 'rev-parse', 'main'), tip)
        self.assertNotIn('backend-v0.1.1', self.run_git(self.remote, 'tag', '--list'))

    def test_version_tag_mismatch_rejected(self):
        self.release()
        (self.repo / 'VERSION').write_text('0.5.0\n')
        self.commit('chore: 错误版本')
        with self.assertRaisesRegex(RuntimeError, '不一致'):
            self.release()


class WorkflowTests(unittest.TestCase):
    def test_trigger_paths_mirror_the_workflow(self):
        # 让位的前提是新提交确实触发了一次发布，所以这里的路径必须和 on.push.paths 一字不差。
        workflow = (Path(__file__).resolve().parents[2] / '.github/workflows/backend-release.yml').read_text()
        block = workflow.split('    paths:\n', 1)[1].split('\n  workflow_dispatch:', 1)[0]
        paths = [line.strip()[2:].strip().strip("'") for line in block.splitlines() if line.strip().startswith('- ')]
        self.assertEqual(backend.TRIGGER_PATHS, paths)


if __name__ == '__main__':
    unittest.main()

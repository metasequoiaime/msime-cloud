"""检查真实引擎任务的路径筛选不会漏掉调用方和依赖原生资源的测试。"""
from pathlib import Path
import re
import unittest


ROOT = Path(__file__).resolve().parents[1]
WORKFLOW = ROOT / '.github/workflows/ci.yml'


class NativeTriggerTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        # 工作流使用的表达式只含 Python 和 POSIX ERE 共有的语法，直接检查实际规则。
        workflow = WORKFLOW.read_text(encoding='utf-8')
        match = re.search(r"grep -qE '([^']+)'", workflow)
        if match is None:
            raise AssertionError('找不到原生任务的路径规则')
        cls.pattern = re.compile(match[1])

    def test_engine_callers_and_verification_paths_trigger_native(self):
        for path in (
            'Dockerfile', 'native/README.md', 'third_party/msime',
            'internal/engine/client.go', 'internal/publictools/tools.go',
            'cmd/msime-cloud/tools_test.go',
            'internal/server/input.go', 'internal/server/input_listed.go',
            'internal/server/handlers.go', 'internal/server/bot_words.go',
            'internal/server/bot_public_tools.go', 'internal/server/bot_shuangpin_keys_test.go',
            'internal/server/word_submissions.go',
            'internal/account/community_resource_apply.go',
            'internal/account/community_resources.go',
            'internal/account/dictionary_manage.go',
            'internal/account/dictionary_query.go',
            'internal/account/dictionary_snapshot_query.go',
            'internal/account/positions.go', 'internal/account/ranking.go',
            'internal/account/snapshot_restore.go',
            'internal/server/testdata/api-coverage.json',
            'scripts/check_api_test_results.py', 'scripts/check_go_coverage.py',
            'scripts/test_api_test_results.py', 'scripts/test_go_coverage.py',
            'scripts/test_native_trigger.py', '.github/workflows/ci.yml',
        ):
            with self.subTest(path=path):
                self.assertRegex(path, self.pattern)

    def test_every_test_using_engine_environment_triggers_native(self):
        found = []
        for directory in ('internal/account', 'internal/server', 'internal/publictools', 'cmd'):
            for path in sorted((ROOT / directory).rglob('*_test.go')):
                source = path.read_text(encoding='utf-8')
                if 'MSIME_ENGINE_TEST_BINARY' in source or 'MSIME_ENGINE_TEST_RESOURCES' in source:
                    found.append(path.relative_to(ROOT).as_posix())
        self.assertTrue(found, '必须找到依赖真实引擎的测试，防止扫描范围意外失效')
        for path in found:
            with self.subTest(path=path):
                self.assertRegex(path, self.pattern)

    def test_direct_engine_queries_trigger_native(self):
        calls = re.compile(r'\b(?:cfg|engine|Engine)\.(?:Query|QuerySnapshot)\(')
        found = []
        for directory in ('internal/account', 'internal/server'):
            for path in sorted((ROOT / directory).glob('*.go')):
                if not path.name.endswith('_test.go') and calls.search(path.read_text(encoding='utf-8')):
                    found.append(path.relative_to(ROOT).as_posix())
        self.assertTrue(found, '必须找到引擎查询调用方')
        for path in found:
            with self.subTest(path=path):
                self.assertRegex(path, self.pattern)

    def test_unrelated_changes_keep_native_task_optional(self):
        for path in (
            'README.md', 'docs/admin.md', 'admin-web/src/main.tsx',
            'internal/account/avatar.go', 'internal/account/preferences_test.go',
            'internal/server/telegram.go', 'internal/server/qq_bot_test.go',
            'cmd/msime-cloud/session.go',
        ):
            with self.subTest(path=path):
                self.assertNotRegex(path, self.pattern)


if __name__ == '__main__':
    unittest.main()

"""验证可导入的 n8n 社交效果周报工作流。"""
import json
from pathlib import Path
import subprocess
import unittest


WORKFLOW = Path(__file__).resolve().parents[2] / "ops/n8n/msime-social-report.json"


class SocialReportWorkflowTests(unittest.TestCase):
    def setUp(self):
        self.workflow = json.loads(WORKFLOW.read_text(encoding="utf-8"))
        self.nodes = {node["name"]: node for node in self.workflow["nodes"]}

    def test_required_nodes_and_connections_exist(self):
        for name in (
            "每周触发",
            "准备查询",
            "查询社交访问",
            "查询安装事件",
            "查询 GitHub 仓库",
            "查询 GitHub 发布",
            "合并发布数据",
            "生成中文周报",
            "发送周报 Telegram",
            "发送周报 Discord",
        ):
            self.assertIn(name, self.nodes)
        for source, outputs in self.workflow["connections"].items():
            self.assertIn(source, self.nodes)
            for branches in outputs.values():
                for branch in branches:
                    for connection in branch:
                        self.assertIn(connection["node"], self.nodes)

    def test_queries_keep_utm_and_install_event(self):
        traffic = self.nodes["查询社交访问"]["parameters"]["url"]
        installs = self.nodes["查询安装事件"]["parameters"]["url"]
        self.assertIn("启用 Umami 访问", self.nodes)
        self.assertIn("跳过 Umami 社交访问", self.nodes)
        self.assertIn("启用 Umami 安装", self.nodes)
        self.assertIn("跳过 Umami 安装事件", self.nodes)
        self.assertIn("utmSource", traffic)
        self.assertIn("utmMedium", traffic)
        self.assertIn("utmCampaign", traffic)
        self.assertIn("breakdown", traffic)
        self.assertIn("startAt", traffic)
        self.assertIn("/breakdown", installs)
        self.assertIn("install_event", installs)

    def test_github_reporting_does_not_require_umami(self):
        code = self.nodes["准备查询"]["parameters"]["jsCode"]
        self.assertIn("MSIME_GITHUB_TOKEN", code)
        self.assertNotIn("MSIME_UMAMI_API_KEY", code)

    def test_report_tracks_snapshot_and_platform_metrics(self):
        code = self.nodes["生成中文周报"]["parameters"]["jsCode"]
        for marker in (
            "$getWorkflowStaticData('global')",
            "last_stars",
            "last_forks",
            "star_delta",
            "platform_rows",
            "utm_source",
            "release_count",
        ):
            self.assertIn(marker, code)

    def test_workflow_does_not_contain_credentials(self):
        text = WORKFLOW.read_text(encoding="utf-8")
        for marker in (
            "bot-token",
            "access-token",
            "app-password",
            "client-secret",
            "ghp_",
            "xoxb-",
        ):
            self.assertNotIn(marker, text)

    def test_code_nodes_are_valid_javascript(self):
        for node in self.workflow["nodes"]:
            if node["type"] != "n8n-nodes-base.code":
                continue
            result = subprocess.run(
                ["node", "--check"],
                input=node["parameters"]["jsCode"],
                text=True,
                capture_output=True,
                check=False,
            )
            self.assertEqual(result.returncode, 0, node["name"] + ": " + result.stderr)


if __name__ == "__main__":
    unittest.main()

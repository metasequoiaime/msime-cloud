"""验证可导入的 n8n 发版分发工作流没有断开的节点或明文凭据。"""
import json
from pathlib import Path
import unittest


WORKFLOW = Path(__file__).resolve().parents[2] / "ops/n8n/msime-release-social.json"


class N8nWorkflowTests(unittest.TestCase):
    def setUp(self):
        self.workflow = json.loads(WORKFLOW.read_text(encoding="utf-8"))
        self.nodes = {node["name"]: node for node in self.workflow["nodes"]}

    def test_required_nodes_and_connections_exist(self):
        for name in (
            "Release webhook",
            "验签和生成文案",
            "发送 Telegram",
            "发送 Discord",
            "发送 Mastodon",
            "Bluesky 创建会话",
            "Bluesky 发帖",
            "发送 X",
            "发送 Reddit",
        ):
            self.assertIn(name, self.nodes)
        for source, outputs in self.workflow["connections"].items():
            self.assertIn(source, self.nodes)
            for branches in outputs.values():
                for branch in branches:
                    for connection in branch:
                        self.assertIn(connection["node"], self.nodes)

    def test_webhook_keeps_raw_body_and_workflow_does_not_contain_credentials(self):
        webhook = self.nodes["Release webhook"]
        self.assertTrue(webhook["parameters"]["options"]["rawBody"])
        code = self.nodes["验签和生成文案"]["parameters"]["jsCode"]
        self.assertIn("X-MSIME-Signature", code)
        self.assertIn("MSIME_SOCIAL_WEBHOOK_SECRET", code)
        text = WORKFLOW.read_text(encoding="utf-8")
        for marker in ("bot-token", "access-token", "app-password", "client-secret"):
            self.assertNotIn(marker, text)


if __name__ == "__main__":
    unittest.main()

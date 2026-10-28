"""验证可导入的 n8n 社交效果周报工作流。"""
import json
from pathlib import Path
import subprocess
import unittest
from urllib.parse import parse_qs, urlsplit


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
        self.assertIn("breakdown", traffic)
        self.assertIn("startAt", traffic)
        self.assertIn("/events/stats", installs)
        self.assertIn("install_event", installs)

    def run_javascript(self, code, context):
        program = """
const fixture = JSON.parse(process.argv[1]);
const state = {};
const lookup = name => ({
  first: () => ({json: fixture.references[name][0]}),
  all: () => fixture.references[name].map(json => ({json})),
});
const result = new Function('$', '$json', '$input', '$getWorkflowStaticData', fixture.code)(
  lookup, fixture.context.json ?? {},
  {all: () => (fixture.context.items ?? []).map(json => ({json}))}, () => state,
);
process.stdout.write(JSON.stringify(result));
"""
        result = subprocess.run(
            ["node", "-e", program, json.dumps({"code": code, "context": context,
                "references": context.get("references", {})})],
            text=True, capture_output=True, check=True,
        )
        return json.loads(result.stdout)

    def test_query_urls_match_umami_api(self):
        context = {"json": {"source": "x & reddit"}, "references": {"准备查询": [{
            "umami_base_url": "https://analytics.example.test",
            "website_id": "website", "start_at": 1000, "end_at": 2000,
            "install_event": "Install / success",
        }]}}
        urls = {}
        for name in ("查询社交访问", "查询安装事件"):
            expression = self.nodes[name]["parameters"]["url"]
            urls[name] = self.run_javascript("return " + expression[3:-3], context)
        traffic = parse_qs(urlsplit(urls["查询社交访问"]).query)
        self.assertEqual(json.loads(traffic["fields"][0]), ["utmSource"])
        self.assertEqual(traffic["utmMedium"], ["social"])
        installs = parse_qs(urlsplit(urls["查询安装事件"]).query)
        self.assertEqual(installs["utmSource"], ["x & reddit"])
        self.assertEqual(installs["event"], ["Install / success"])

    def test_queries_keep_empty_and_array_responses_in_one_item(self):
        for name in ("查询社交访问", "查询安装事件", "查询 GitHub 仓库", "查询 GitHub 发布"):
            self.assertTrue(self.nodes[name]["parameters"]["options"]["response"]["response"]["fullResponse"])
            self.assertTrue(self.nodes[name]["alwaysOutputData"])

    def test_install_sources_are_unique_and_empty_traffic_still_continues(self):
        code = self.nodes["准备安装查询"]["parameters"]["jsCode"]
        sources = self.run_javascript(code, {"json": {"body": [
            {"utmSource": "x"}, {"utmSource": "x"}, {"utmSource": "reddit"},
        ]}})
        self.assertEqual([item["json"]["source"] for item in sources], ["x", "reddit"])
        self.assertEqual(self.run_javascript(code, {"json": {"body": []}}), [{"json": {"source": ""}}])

    def test_install_counts_use_event_stats_and_preserve_sources(self):
        result = self.run_javascript(self.nodes["整理安装数据"]["parameters"]["jsCode"], {
            "items": [{"body": {"data": {"events": 3, "visitors": 1}}},
                      {"body": {"data": {"events": 5, "visitors": 2}}}],
            "references": {"准备安装查询": [{"source": "x"}, {"source": "reddit"}], "准备查询": [{}]},
        })
        self.assertEqual(result[0]["json"]["installs"], [
            {"utmSource": "x", "events": 3}, {"utmSource": "reddit", "events": 5},
        ])

    def test_report_keeps_all_releases_and_marks_query_errors(self):
        result = self.run_javascript(self.nodes["生成中文周报"]["parameters"]["jsCode"], {"json": {
            "period": {"period_start": "2026-10-21", "period_end": "2026-10-28"},
            "traffic": {"error": "upstream failed"}, "installs": {"error": "upstream failed"},
            "github": {"body": {"stargazers_count": 100, "forks_count": 20}},
            "releases": {"body": [{"tag_name": "v1", "published_at": "2026-10-23"},
                                  {"tag_name": "v2", "published_at": "2026-10-24"}]},
        }})[0]["json"]
        self.assertEqual(result["release_count"], 2)
        self.assertIn("官网访问查询失败", result["report"])
        self.assertIn("安装事件查询失败", result["report"])

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

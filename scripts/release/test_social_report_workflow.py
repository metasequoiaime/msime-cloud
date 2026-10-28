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
            "查询转化事件",
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

    def test_queries_keep_utm_and_conversion_event(self):
        traffic = self.nodes["查询社交访问"]["parameters"]["url"]
        installs = self.nodes["查询转化事件"]["parameters"]["url"]
        self.assertIn("启用 Umami 访问", self.nodes)
        self.assertIn("跳过 Umami 社交访问", self.nodes)
        self.assertIn("启用 Umami 转化", self.nodes)
        self.assertIn("跳过 Umami 转化事件", self.nodes)
        self.assertIn("utmSource", traffic)
        self.assertIn("utmMedium", traffic)
        self.assertIn("breakdown", traffic)
        self.assertIn("startAt", traffic)
        self.assertIn("/events/stats", installs)
        self.assertIn("$json.event", installs)

    def run_javascript(self, code, context):
        program = """
const fixture = JSON.parse(process.argv[1]);
const state = fixture.context.state ?? {};
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
        context = {"json": {"source": "x & reddit", "event": "Install / success"}, "references": {"准备查询": [{
            "umami_base_url": "https://analytics.example.test",
            "website_id": "website", "start_at": 1000, "end_at": 2000,
            "install_event": "Install / success",
        }]}}
        urls = {}
        for name in ("查询社交访问", "查询转化事件"):
            expression = self.nodes[name]["parameters"]["url"]
            urls[name] = self.run_javascript("return " + expression[3:-3], context)
        traffic = parse_qs(urlsplit(urls["查询社交访问"]).query)
        self.assertEqual(json.loads(traffic["fields"][0]), ["utmSource"])
        self.assertEqual(traffic["utmMedium"], ["social"])
        installs = parse_qs(urlsplit(urls["查询转化事件"]).query)
        self.assertEqual(installs["utmSource"], ["x & reddit"])
        self.assertEqual(installs["event"], ["Install / success"])

    def test_queries_keep_empty_and_array_responses_in_one_item(self):
        for name in ("查询社交访问", "查询转化事件", "查询 GitHub 仓库", "查询 GitHub 发布"):
            self.assertTrue(self.nodes[name]["parameters"]["options"]["response"]["response"]["fullResponse"])
            self.assertTrue(self.nodes[name]["alwaysOutputData"])

    def test_install_sources_are_unique_and_empty_traffic_still_continues(self):
        code = self.nodes["准备转化查询"]["parameters"]["jsCode"]
        sources = self.run_javascript(code, {"references": {"准备查询": [{"install_enabled": False}]}, "json": {"body": [
            {"utmSource": "x"}, {"utmSource": "x"}, {"utmSource": "reddit"},
        ]}})
        self.assertEqual([item["json"]["source"] for item in sources], ["x", "reddit"])
        self.assertEqual(self.run_javascript(code, {"json": {"body": []}, "references": {"准备查询": [{}]}}), [{"json": {"source": "", "kind": "downloads", "event": "Download"}}])

    def test_install_counts_use_event_stats_and_preserve_sources(self):
        result = self.run_javascript(self.nodes["整理转化数据"]["parameters"]["jsCode"], {
            "items": [{"body": {"data": {"events": 3, "visitors": 1}}},
                      {"body": {"data": {"events": 5, "visitors": 2}}}],
            "references": {"准备转化查询": [{"source": "x", "kind": "installs"}, {"source": "reddit", "kind": "installs"}], "准备查询": [{"umami_enabled": True, "install_enabled": True}]},
        })
        self.assertEqual(result[0]["json"]["installs"], [
            {"utmSource": "x", "events": 3}, {"utmSource": "reddit", "events": 5},
        ])

    def test_report_keeps_all_releases_and_marks_query_errors(self):
        result = self.run_javascript(self.nodes["生成中文周报"]["parameters"]["jsCode"], {"json": {
            "period": {"period_start": "2026-10-21", "period_end": "2026-10-28", "start_at": 1792540800000, "end_at": 1793145600000, "install_enabled": True},
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

    def test_downloads_are_queried_by_default_and_installs_require_opt_in(self):
        code = self.nodes["准备转化查询"]["parameters"]["jsCode"]
        for enabled in (False, True):
            result = self.run_javascript(code, {"json": {"body": [{"utmSource": "x"}]},
                "references": {"准备查询": [{"install_enabled": enabled, "install_event": "Install"}]}})
            self.assertEqual([item["json"]["event"] for item in result], ["Download", "Install"] if enabled else ["Download"])

    def test_conversion_failures_are_not_reported_as_zero_and_skipped_queries_continue(self):
        code = self.nodes["整理转化数据"]["parameters"]["jsCode"]
        period = {"umami_enabled": True, "install_enabled": True}
        result = self.run_javascript(code, {"items": [
            {"statusCode": 200, "body": {"data": {"events": 3}}},
            {"error": "upstream failed"}, {"body": {"unexpected": True}},
        ], "references": {"准备查询": [period], "准备转化查询": [
            {"source": "x", "kind": "downloads"}, {"source": "reddit", "kind": "downloads"},
            {"source": "x", "kind": "installs"},
        ]}})[0]["json"]
        self.assertEqual(result["downloads"], {"error": True})
        self.assertEqual(result["installs"], {"error": True})
        skipped = self.run_javascript(code, {"references": {"准备查询": [{"umami_enabled": False}]}})[0]["json"]
        self.assertIsNone(skipped["downloads"])
        self.assertIsNone(skipped["installs"])

    def test_download_and_install_counts_remain_separate_across_sources(self):
        references = {"准备查询": [{"umami_enabled": True, "install_enabled": True}]}
        queries = self.run_javascript(self.nodes["准备转化查询"]["parameters"]["jsCode"], {
            "json": {"body": [{"utmSource": "x"}, {"utmSource": "reddit"}]},
            "references": {"准备查询": [{"install_enabled": True, "install_event": "Install"}]},
        })
        references["准备转化查询"] = [item["json"] for item in queries]
        result = self.run_javascript(self.nodes["整理转化数据"]["parameters"]["jsCode"], {
            "items": [{"body": {"data": {"events": count}}} for count in (8, 2, 5, 1)],
            "references": references,
        })[0]["json"]
        self.assertEqual(result["downloads"], [{"utmSource": "x", "events": 8}, {"utmSource": "reddit", "events": 5}])
        self.assertEqual(result["installs"], [{"utmSource": "x", "events": 2}, {"utmSource": "reddit", "events": 1}])

    def test_report_distinguishes_clicks_from_installs_and_uses_real_newlines(self):
        result = self.run_javascript(self.nodes["生成中文周报"]["parameters"]["jsCode"], {"json": {
            "period": {"install_enabled": False},
            "traffic": {"body": [{"utmSource": "x", "visitors": 2, "visits": 3}]},
            "downloads": [{"utmSource": "x", "events": 5}], "installs": None,
            "github": {"body": {"stargazers_count": 100, "forks_count": 20}}, "releases": {"body": []},
        }})[0]["json"]
        self.assertEqual(result["platform_rows"][0], {"source": "x", "visitors": 2, "visits": 3, "downloads": 5, "installs": None})
        self.assertIn("下载点击 5", result["report"])
        self.assertIn("安装数据尚未接入", result["report"])
        self.assertNotIn("安装事件 0", result["report"])
        self.assertIn("\n\n平台数据", result["report"])
        self.assertNotIn("\\n", result["report"])

    def test_github_failure_does_not_create_fake_zero_counts_or_negative_growth(self):
        result = self.run_javascript(self.nodes["生成中文周报"]["parameters"]["jsCode"], {
            "state": {"last_stars": 100, "last_forks": 20},
            "json": {"github": {"error": "unavailable"}, "releases": {"error": "unavailable"}},
        })[0]["json"]
        self.assertIsNone(result["star_delta"])
        self.assertIsNone(result["fork_delta"])
        self.assertIsNone(result["release_count"])
        self.assertIn("GitHub 仓库数据不可用", result["report"])
        self.assertNotIn("Star：0", result["report"])

    def test_release_filter_uses_exact_period_and_snapshot_delta_uses_previous_success(self):
        result = self.run_javascript(self.nodes["生成中文周报"]["parameters"]["jsCode"], {
            "state": {"last_stars": 99, "last_forks": 19}, "json": {
                "period": {"start_at": 1000, "end_at": 2000},
                "github": {"body": {"stargazers_count": 100, "forks_count": 20}},
                "releases": {"body": [
                    {"tag_name": "too-old", "published_at": "1970-01-01T00:00:00.999Z"},
                    {"tag_name": "start", "published_at": "1970-01-01T00:00:01.000Z"},
                    {"tag_name": "end", "published_at": "1970-01-01T00:00:02.000Z"},
                    {"tag_name": "future", "published_at": "1970-01-01T00:00:02.001Z"},
                ]},
            },
        })[0]["json"]
        self.assertEqual(result["release_count"], 2)
        self.assertEqual(result["star_delta"], 1)
        self.assertEqual(result["fork_delta"], 1)
        self.assertIn("较上次快照 +1", result["report"])
        self.assertNotIn("too-old", result["report"])
        self.assertNotIn("future", result["report"])

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

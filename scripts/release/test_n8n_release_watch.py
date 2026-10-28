"""验证客户端正式版筛选、签名、去重和不发帖验收。"""
import hashlib
import hmac
import json
from pathlib import Path
import subprocess
import unittest


ROOT = Path(__file__).resolve().parents[2]
WATCH = ROOT / "ops/n8n/msime-release-watch.json"
SOCIAL = ROOT / "ops/n8n/msime-release-social.json"


class ReleaseWatchTests(unittest.TestCase):
    def run_fixture(self, fixture):
        program = r"""
const crypto = require('crypto');
const fixture = JSON.parse(process.argv[1]);
const fs = require('fs');
const watch = JSON.parse(fs.readFileSync(process.argv[2], 'utf8'));
const social = JSON.parse(fs.readFileSync(process.argv[3], 'utf8'));
const select = watch.nodes.find(n => n.name === '筛选并签名正式版本').parameters.jsCode;
const dispatch = social.nodes.find(n => n.name === '验签和生成文案').parameters.jsCode;
const now = Date.now();
const state = {enabledAt: new Date(now - 3600000).toISOString()};
const env = {MSIME_SOCIAL_WEBHOOK_SECRET: 'synthetic-secret', MSIME_X_ENABLED: '1'};
if (fixture.dry_run) env.MSIME_RELEASE_WATCH_DRY_RUN = '1';
const make = item => ({tag_name: item.tag, name: item.tag, body: '- 合成更新',
  html_url: 'https://github.com/example/project/releases/tag/' + item.tag,
  published_at: new Date(now - (item.age_minutes ?? 5) * 60000).toISOString(),
  draft: false, prerelease: false, assets: [{state: 'uploaded', size: 100}], ...item});
const sources = ['metasequoiaime/msime', 'metasequoiaime/msime-windows'];
const inputs = sources.map((repository, index) => ({json: fixture.failed_source === index
  ? {error: true, statusCode: 503}
  : {statusCode: 200, body: (fixture.releases[index] || []).map(make)}}));
const selected = new Function('$input', '$', '$env', '$getWorkflowStaticData', 'require', select)(
  {all: () => inputs}, () => ({all: () => sources.map(repository => ({json: {repository}}))}),
  env, () => state, require);
if (fixture.direct_payload) {
  const body = JSON.stringify(fixture.direct_payload);
  selected.push({json: {body, signature: 'sha256=' + crypto.createHmac('sha256', env.MSIME_SOCIAL_WEBHOOK_SECRET).update(body).digest('hex')}});
}
const dispatchState = fixture.completed ? {completed: fixture.completed} : {};
const posts = selected.flatMap(item => new Function('$input', '$env', '$getWorkflowStaticData', 'require', 'Buffer', dispatch)(
  {first: () => ({json: {rawBody: item.json.body, headers: {'x-msime-signature': item.json.signature}}})},
  env, () => dispatchState, require, Buffer));
process.stdout.write(JSON.stringify({selected, posts, state, dispatchState}));
"""
        # 工作流文件按路径交给 node：两份 JSON 原文作命令行参数会超过 Windows 的命令行长度上限（WinError 206）。
        result = subprocess.run(
            ["node", "-e", program, json.dumps(fixture), str(WATCH), str(SOCIAL)],
            capture_output=True, text=True, check=True,
        )
        return json.loads(result.stdout)

    def test_all_client_platforms_and_signatures(self):
        tags = [f"{name}-v1.2.3" for name in ("windows7", "windows", "macos", "linux", "android", "ios", "harmony")]
        result = self.run_fixture({"releases": [[{"tag": tag} for tag in tags], [{"tag": "v1.2.3"}]]})
        self.assertEqual(len(result["selected"]), 8)
        self.assertEqual(len(result["posts"]), 8)
        for selected in result["selected"]:
            envelope = selected["json"]
            expected = "sha256=" + hmac.new(b"synthetic-secret", envelope["body"].encode(), hashlib.sha256).hexdigest()
            self.assertEqual(envelope["signature"], expected)
        for post in result["posts"]:
            self.assertEqual(post["json"]["channel"], "x")
            self.assertNotIn("后端", post["json"]["text"])

    def test_excludes_previews_history_empty_assets_and_other_tags(self):
        excluded = [
            {"tag": "android-v1.2.3-beta"}, {"tag": "android-v1.2.3-alpha"},
            {"tag": "nightly"}, {"tag": "backend-v1.2.3"},
            {"tag": "android-v1.2.3", "prerelease": True},
            {"tag": "android-v1.2.3", "draft": True},
            {"tag": "android-v1.2.3", "age_minutes": 90},
            {"tag": "android-v1.2.3", "age_minutes": -5},
            {"tag": "android-v1.2.3", "assets": []},
            {"tag": "android-v1.2.3", "assets": [{"state": "new", "size": 100}]},
            {"tag": "android-v1.2.3", "assets": [{"state": "uploaded", "size": 0}]},
        ]
        result = self.run_fixture({"releases": [excluded, [{"tag": "macos-v1.2.3"}]]})
        self.assertEqual(result["selected"], [])
        self.assertEqual(result["posts"], [])

    def test_dry_run_validates_signed_delivery_without_sending(self):
        result = self.run_fixture({"dry_run": True, "releases": [[{"tag": "android-v1.2.3"}], []]})
        self.assertEqual(result["posts"][0]["json"]["channel"], "probe")
        self.assertEqual(result["posts"][0]["json"]["channels"], ["x"])
        self.assertEqual(result["dispatchState"]["lastVerification"]["platform"], "Android")
        self.assertNotIn("completed", result["dispatchState"])

    def test_repeated_checks_do_not_send_completed_platforms(self):
        result = self.run_fixture({"releases": [[{"tag": "android-v1.2.3"}], []],
            "completed": {"metasequoiaime/msime:android-v1.2.3": {"x": "synthetic"}}})
        self.assertEqual(len(result["selected"]), 1)
        self.assertEqual(result["posts"], [])

    def test_dispatcher_also_rejects_out_of_scope_signed_events(self):
        for repository, tag, flags in [
            ("metasequoiaime/msime-cloud", "backend-v1.2.3", {}),
            ("example/project", "android-v1.2.3", {}),
            ("metasequoiaime/msime", "android-v1.2.3-beta", {}),
            ("metasequoiaime/msime", "android-v1.2.3", {"prerelease": True}),
            ("metasequoiaime/msime", "android-v1.2.3", {"draft": True}),
        ]:
            with self.subTest(repository=repository, tag=tag, flags=flags):
                result = self.run_fixture({"releases": [[], []], "direct_payload": {
                    "schema_version": 1, "event": "release.published", "repository": repository,
                    "idempotency_key": f"{repository}:{tag}", "release": {"tag": tag, **flags},
                }})
                self.assertEqual(result["posts"], [])

    def test_one_failed_repository_does_not_hide_the_other(self):
        result = self.run_fixture({"failed_source": 0, "releases": [[], [{"tag": "v1.2.3"}]]})
        self.assertEqual(len(result["posts"]), 1)
        self.assertEqual(result["state"]["sourceErrors"]["metasequoiaime/msime"]["status"], 503)

    def test_schedule_and_internal_raw_delivery(self):
        watch = json.loads(WATCH.read_text())
        nodes = {node["name"]: node for node in watch["nodes"]}
        self.assertEqual(nodes["每五分钟检查"]["parameters"]["rule"]["interval"],
            [{"field": "minutes", "minutesInterval": 5}])
        relay = nodes["分发正式客户端版本"]["parameters"]
        self.assertEqual(relay["url"], "http://127.0.0.1:5678/webhook/msime-release")
        self.assertEqual(relay["contentType"], "raw")
        self.assertEqual(relay["options"]["batching"]["batch"]["batchSize"], 1)
        social = json.loads(SOCIAL.read_text())
        webhook = next(node for node in social["nodes"] if node["name"] == "Release webhook")
        self.assertEqual(webhook["parameters"]["responseMode"], "lastNode")
        self.assertEqual(social["connections"]["验收请求"]["main"][0][0]["node"], "返回验收结果")
        for source, outputs in watch["connections"].items():
            self.assertIn(source, nodes)
            for branch in outputs["main"]:
                for connection in branch:
                    self.assertIn(connection["node"], nodes)


if __name__ == "__main__":
    unittest.main()

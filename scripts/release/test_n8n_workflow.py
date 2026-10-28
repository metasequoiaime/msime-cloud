"""验证可导入的 n8n 发版分发工作流没有断开的节点或明文凭据。"""
import json
from pathlib import Path
import subprocess
import unittest
from urllib.parse import parse_qs, urlsplit


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
        self.assertIn("utm_source", code)
        self.assertIn("utm_medium=social", code)
        self.assertIn("utm_campaign", code)
        text = WORKFLOW.read_text(encoding="utf-8")
        for marker in ("bot-token", "access-token", "app-password", "client-secret"):
            self.assertNotIn(marker, text)

    def build_posts(self, landing=None):
        program = """
const crypto = require('crypto');
const workflow = JSON.parse(process.argv[1]);
const payload = {schema_version: 1, event: 'release.published',
  idempotency_key: 'test/repo:backend-v1.2.3', repository: 'test/repo',
  release: {tag: 'backend-v1.2.3', body: '- ' + '合成更新'.repeat(5000), url: 'https://github.com/test/repo/releases/tag/backend-v1.2.3'}};
const rawBody = JSON.stringify(payload);
const env = {MSIME_SOCIAL_WEBHOOK_SECRET: 'synthetic-secret'};
for (const channel of ['TELEGRAM', 'DISCORD', 'MASTODON', 'BLUESKY', 'X', 'REDDIT']) env['MSIME_' + channel + '_ENABLED'] = '1';
for (const key of ['TELEGRAM_BOT_TOKEN','TELEGRAM_CHAT_ID','DISCORD_WEBHOOK_URL','MASTODON_INSTANCE','MASTODON_ACCESS_TOKEN','BLUESKY_IDENTIFIER','BLUESKY_APP_PASSWORD','REDDIT_ACCESS_TOKEN','REDDIT_SUBREDDIT','REDDIT_USER_AGENT']) env['MSIME_' + key] = 'synthetic';
if (process.argv[2]) env.MSIME_SOCIAL_SITE_URL = process.argv[2];
const item = {json: {rawBody, headers: {'x-msime-signature': 'sha256=' + crypto.createHmac('sha256', env.MSIME_SOCIAL_WEBHOOK_SECRET).update(rawBody).digest('hex')}}};
const code = workflow.nodes.find(node => node.name === '验签和生成文案').parameters.jsCode;
const posts = new Function('$input', '$env', '$getWorkflowStaticData', 'require', 'Buffer', code)(
  {first: () => item}, env, () => ({}), require, Buffer);
process.stdout.write(JSON.stringify(posts));
"""
        return subprocess.run(["node", "-e", program, json.dumps(self.workflow), landing or ""], text=True, capture_output=True)

    def test_all_platforms_keep_complete_trackable_links_when_release_notes_are_long(self):
        result = self.build_posts()
        self.assertEqual(result.returncode, 0, result.stderr)
        posts = [item["json"] for item in json.loads(result.stdout)]
        self.assertEqual(len(posts), 6)
        limits = {"telegram": 4096, "discord": 2000, "mastodon": 500, "bluesky": 300, "x": 280, "reddit": 40000}
        for post in posts:
            link = post["tracking_url"]
            parsed = urlsplit(link)
            self.assertEqual(parsed.netloc, "msime.app")
            self.assertEqual(parsed.path, "/download/")
            query = parse_qs(parsed.query)
            self.assertEqual(query["utm_source"], [post["channel"]])
            self.assertEqual(query["utm_medium"], ["social"])
            self.assertRegex(query["utm_campaign"][0], r"^[a-zA-Z0-9._~-]{1,128}$")
            self.assertIn(link, post["text"])
            self.assertLessEqual(len(post["text"]), limits[post["channel"]])
            if post["channel"] in ("telegram", "discord", "reddit"):
                self.assertIn("更新说明：https://github.com/test/repo/releases/tag/backend-v1.2.3", post["text"])

    def test_landing_url_is_configurable_and_rejects_credentials_or_query_strings(self):
        result = self.build_posts("https://example.test/download/")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(json.loads(result.stdout)[0]["json"]["tracking_url"].startswith("https://example.test/download/?"))
        for invalid in ("http://example.test/", "https://user:password@example.test/", "https://example.test/?token=private"):
            self.assertNotEqual(self.build_posts(invalid).returncode, 0)

    def test_binary_raw_body_preserves_signature_and_rejects_tampering(self):
        program = """
const crypto = require('crypto');
const code = JSON.parse(process.argv[1]).nodes.find(n => n.name === '验签和生成文案').parameters.jsCode;
const raw = '{ "schema_version": 1, "event": "release.published", "idempotency_key": "test:1", "release": {"tag": "backend-v1", "body": "- 中文更新"} }';
const secret = 'synthetic-secret';
const signature = 'sha256=' + crypto.createHmac('sha256', secret).update(raw).digest('hex');
const execute = (body, binary) => new Function('$input', '$env', '$getWorkflowStaticData', 'require', 'Buffer', code)(
  {first: () => ({json: {headers: {'x-msime-signature': signature}, body: JSON.parse(body)},
    ...(binary ? {binary: {data: {data: Buffer.from(body).toString('base64')}}} : {})})},
  {MSIME_SOCIAL_WEBHOOK_SECRET: secret, MSIME_X_ENABLED: '1'}, () => ({}), require, Buffer);
const posts = execute(raw, true);
let rejected = 0;
for (const [body, binary] of [[raw.replace('中文更新', '篡改更新'), true], [raw, false]]) {
  try {execute(body, binary);} catch {rejected++;}
}
process.stdout.write(JSON.stringify({posts, rejected}));
"""
        result = subprocess.run(["node", "-e", program, json.dumps(self.workflow)],
            text=True, capture_output=True, check=True)
        data = json.loads(result.stdout)
        self.assertEqual(data["rejected"], 2)
        self.assertEqual(len(data["posts"]), 1)
        self.assertIn("• 中文更新", data["posts"][0]["json"]["text"])

    def test_x_uses_refreshable_n8n_oauth2_credentials(self):
        node = self.nodes["发送 X"]
        self.assertEqual(node["parameters"]["authentication"], "genericCredentialType")
        self.assertEqual(node["parameters"]["genericAuthType"], "oAuth2Api")
        self.assertEqual(node["credentials"]["oAuth2Api"]["id"], "msimeXOAuth2")
        self.assertNotIn("headerParameters", node["parameters"])
        self.assertNotIn("MSIME_X_ACCESS_TOKEN", WORKFLOW.read_text(encoding="utf-8"))

    def test_bluesky_session_and_utf8_link_facets(self):
        program = """
const workflow = JSON.parse(process.argv[1]);
const code = workflow.nodes.find(n => n.name === '准备 Bluesky 帖子').parameters.jsCode;
const url = 'https://example.test/download/?utm_source=bluesky';
const source = {text: '水杉输入法 🌲 新版已发布。\\n下载：' + url, tracking_url: url};
const execute = response => new Function('$json', '$', 'Buffer', code)(response,
  name => ({item: {json: source}}), Buffer);
const session = {accessJwt: 'synthetic-session', did: 'did:plc:synthetic'};
const good = execute({statusCode: 200, body: session})[0].json;
const facet = good.record.facets[0];
const linked = Buffer.from(good.record.text).subarray(facet.index.byteStart, facet.index.byteEnd).toString('utf8');
const failures = [{statusCode: 401, body: {error: 'AuthenticationRequired'}},
  {error: {message: 'synthetic'}}, {statusCode: 200, body: {}},
  {statusCode: 200, body: {...session, did: 'invalid'}},
  {statusCode: 200, body: {...session, accessJwt: ''}}].map(execute);
source.text = '缺少推广链接';
failures.push(execute({statusCode: 200, body: session}));
process.stdout.write(JSON.stringify({good, linked, failures}));
"""
        result = subprocess.run(["node", "-e", program, json.dumps(self.workflow)],
            text=True, capture_output=True, check=True)
        data = json.loads(result.stdout)
        self.assertEqual(data["good"]["repo"], "did:plc:synthetic")
        self.assertEqual(data["good"]["accessJwt"], "synthetic-session")
        facet = data["good"]["record"]["facets"][0]
        self.assertEqual(data["linked"], facet["features"][0]["uri"])
        self.assertEqual(data["good"]["record"]["langs"], ["zh"])
        for failure in data["failures"]:
            self.assertEqual(failure, [{"json": {"error": True}}])
        routes = self.workflow["connections"]["Bluesky 会话有效"]["main"]
        self.assertEqual(routes[0][0]["node"], "Bluesky 发帖")
        self.assertEqual(routes[1][0]["node"], "记录 Bluesky 成功")

    def test_social_success_requires_platform_post_identifiers(self):
        program = """
const workflow = JSON.parse(process.argv[1]);
const results = [];
for (const [name, key, channel] of [['Bluesky', 'uri', 'bluesky'], ['Mastodon', 'id', 'mastodon']]) {
  const code = workflow.nodes.find(n => n.name === '记录 ' + name + ' 成功').parameters.jsCode;
  for (const [response, expected] of [[{error: true}, false], [{statusCode: 200, body: {}}, false],
      [{statusCode: 500, body: {[key]: 'synthetic'}}, false],
      [{statusCode: 200, body: {[key]: 'synthetic'}}, true]]) {
    const state = {};
    const output = new Function('$json', '$', '$getWorkflowStaticData', code)(response,
      name => ({item: {json: {idempotency_key: 'synthetic:1'}}}), () => state);
    results.push({ok: output[0].json.ok, marked: Boolean(state.completed?.['synthetic:1']?.[channel]), expected});
  }
}
process.stdout.write(JSON.stringify(results));
"""
        result = subprocess.run(["node", "-e", program, json.dumps(self.workflow)],
            text=True, capture_output=True, check=True)
        for case in json.loads(result.stdout):
            self.assertEqual(case["ok"], case["expected"])
            self.assertEqual(case["marked"], case["expected"])
        headers = self.nodes["发送 Mastodon"]["parameters"]["headerParameters"]["parameters"]
        self.assertIn({"name": "Idempotency-Key", "value": "={{ $json.idempotency_key }}"}, headers)


if __name__ == "__main__":
    unittest.main()

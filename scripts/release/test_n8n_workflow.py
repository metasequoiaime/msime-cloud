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
  idempotency_key: 'metasequoiaime/msime:android-v1.2.3', repository: 'metasequoiaime/msime',
  release: {tag: 'android-v1.2.3', body: '- ' + '合成更新'.repeat(5000), url: 'https://github.com/metasequoiaime/msime/releases/tag/android-v1.2.3'}};
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
                self.assertIn("更新说明：https://github.com/metasequoiaime/msime/releases/tag/android-v1.2.3", post["text"])

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
const raw = '{ "schema_version": 1, "event": "release.published", "idempotency_key": "metasequoiaime/msime:android-v1.2.3", "repository": "metasequoiaime/msime", "release": {"tag": "android-v1.2.3", "body": "- **中文更新**：说明"} }';
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
        self.assertTrue(data["posts"][0]["json"]["text"].startswith("水杉输入法 Android v1.2.3 已发布：中文更新\n"))

    def short_posts(self, body):
        program = """
const crypto = require('crypto');
const workflow = JSON.parse(process.argv[1]);
const payload = {schema_version: 1, event: 'release.published',
  idempotency_key: 'metasequoiaime/msime:macos-v1.2.3', repository: 'metasequoiaime/msime',
  release: {tag: 'macos-v1.2.3', body: process.argv[2], url: 'https://github.com/metasequoiaime/msime/releases/tag/macos-v1.2.3'}};
const rawBody = JSON.stringify(payload);
const env = {MSIME_SOCIAL_WEBHOOK_SECRET: 'synthetic-secret', MSIME_X_ENABLED: '1', MSIME_MASTODON_ENABLED: '1', MSIME_BLUESKY_ENABLED: '1'};
for (const key of ['MASTODON_INSTANCE','MASTODON_ACCESS_TOKEN','BLUESKY_IDENTIFIER','BLUESKY_APP_PASSWORD']) env['MSIME_' + key] = 'synthetic';
const item = {json: {rawBody, headers: {'x-msime-signature': 'sha256=' + crypto.createHmac('sha256', env.MSIME_SOCIAL_WEBHOOK_SECRET).update(rawBody).digest('hex')}}};
const code = workflow.nodes.find(node => node.name === '验签和生成文案').parameters.jsCode;
const posts = new Function('$input', '$env', '$getWorkflowStaticData', 'require', 'Buffer', code)(
  {first: () => item}, env, () => ({}), require, Buffer);
process.stdout.write(JSON.stringify(posts));
"""
        result = subprocess.run(["node", "-e", program, json.dumps(self.workflow), body], text=True, capture_output=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        return {item["json"]["channel"]: item["json"] for item in json.loads(result.stdout)}

    @staticmethod
    def x_weight(text):
        weight = 0
        for word in text.replace("\n", " \n ").split(" "):
            if word.startswith(("https://", "http://")):
                weight += 23
                continue
            for char in word:
                code = ord(char)
                weight += 1 if code <= 0x10FF or 0x2000 <= code <= 0x200D or 0x2010 <= code <= 0x201F or 0x2032 <= code <= 0x2037 else 2
            weight += 1
        return weight - 1

    # X、Bluesky、Mastodon 不渲染 Markdown：短帖只列功能名，不带加粗、反引号、链接语法、列表符号和 issue 编号，开发与感谢下的条目不列也不计数。
    def test_short_posts_are_plain_text_without_markdown(self):
        body = """<!-- msime-download-guide -->
## 下载

| 版本 | 安装包 |
| --- | --- |
| **水杉输入法** | [msime.pkg](https://example.test/msime.pkg) |

- 这一条在下载说明里，不是更新。
<!-- /msime-download-guide -->

## v1.2.3 更新内容

### ✨ 新功能

- **PLaMo 离线翻译**：输入法内置小模型，可以用 `msime-mcp` 调用。(#10965)
- **[快捷短语](https://example.test/doc)光标标记**：可以写 `${cursor}`。(#10539)
- 新增 `Fedora 45` 软件源，Fedora 45 用户可以通过它安装。(#11084)
  - 缩进的子项不单独列出。
- 设置页的插件列表可以直接开关插件，也可以调整插件的顺序。(#11222)

### 🐛 问题修复

- 修复设置应用一直开着时，「我的词条」的增删改不生效的问题。(感谢 @someone，#11099，修复 #11098)
- 修复悬浮工具栏大小来回抖动的问题。

### 🔧 开发与构建

- 恢复 Nightly 的编译。(#11167)

### 🙏 感谢

- 问题反馈：#11098 的各位报告者
"""
        posts = self.short_posts(body)
        self.assertEqual(set(posts), {"x", "bluesky", "mastodon"})
        for channel, post in posts.items():
            line, link = post["text"].split("\n")
            self.assertEqual(line, "水杉输入法 macOS v1.2.3 已发布：PLaMo 离线翻译、快捷短语光标标记、新增 Fedora 45 软件源等 6 项更新")
            self.assertEqual(link, post["tracking_url"])
            for mark in ("**", "`", "[", "](", "#", "•", "- ", "|", "<", "$"):
                self.assertNotIn(mark, line, channel)
            self.assertLessEqual(len(line), 140)
            self.assertLessEqual(self.x_weight(post["text"]), 280)

    def test_short_posts_list_at_most_three_items_and_fall_back_to_a_count(self):
        # 最多列三条并注明总数，超过 18 字的不列；没有可列的要点时只写数量；没有任何条目时只写已发布；要点自带顿号时用分号隔开。
        items = "\n".join(f"- **功能{n}**：说明" for n in "一二三四五六") + "\n- **" + "很长的功能名称" * 3 + "**：说明"
        self.assertEqual(self.short_posts(items)["x"]["text"].split("\n")[0], "水杉输入法 macOS v1.2.3 已发布：功能一、功能二、功能三等 7 项更新")
        self.assertEqual(self.short_posts("- 设置页可以调整顺序，也可以开关。")["x"]["text"].split("\n")[0], "水杉输入法 macOS v1.2.3 已发布，含 1 项更新")
        self.assertEqual(self.short_posts("没有列表的说明")["x"]["text"].split("\n")[0], "水杉输入法 macOS v1.2.3 已发布")
        self.assertEqual(self.short_posts("- **全拼 14 键、18 键**：说明\n- **五笔 14 键**：说明")["x"]["text"].split("\n")[0], "水杉输入法 macOS v1.2.3 已发布：全拼 14 键、18 键；五笔 14 键")

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

# 发版社交分发

后端版本与镜像发布工作流在镜像构建、GitHub Release 创建以及 `msime-cloud` 命令行资产上传完成后，执行最后的 `announce` job。这个 job 不直接保存各个平台的账号令牌，只把完整 Release 封装成一份带 HMAC 签名的 JSON，发送到社交分发 webhook。

## 配置 GitHub Secrets

在仓库的 Settings → Secrets and variables → Actions 中新增：

| Secret | 内容 |
| --- | --- |
| `SOCIAL_WEBHOOK_URL` | n8n、Make 或自建分发服务的 HTTPS webhook 地址 |
| `SOCIAL_WEBHOOK_SECRET` | 与分发服务约定的随机共享密钥 |

两个 Secret 都配置后，下一次正式 Release 会自动发送。只配置一个时 workflow 会跳过分发，不会影响镜像或 Release 发布。密钥只在 Actions 环境变量中使用，不要写进仓库、Release 说明或日志。

## Webhook 合约

请求方法是 `POST`，正文是 UTF-8 JSON，带有以下请求头：

- `X-MSIME-Signature: sha256=<HMAC-SHA256>`：对原始请求正文用 `SOCIAL_WEBHOOK_SECRET` 计算。
- `X-MSIME-Idempotency-Key: metasequoiaime/msime-cloud:backend-v1.2.3`：下游必须按此键去重。
- `Content-Type: application/json`。

正文包含 `schema_version`、`event`、`idempotency_key`、`repository` 和 `release`。`release` 中有 `tag`、`name`、`body`、`url`、`published_at` 和 `target_commitish`。下游应先验签和去重，再按平台限制生成短帖、长帖或线程。

分发服务建议把 Telegram、Discord、Mastodon 和 Bluesky 设为自动发布，把 X、微博、小红书和 Reddit 设为人工审核或延迟发布。每个平台单独记录发布结果，失败时只重试网络错误和 5xx；不要把上游响应正文原样转发到日志或帖子。

## 手动补发

在 Actions 页面重新运行同一个发版 workflow 会复用已有标签并重新构建缺失的 Release 资产。社交分发以版本标签为幂等键，下游应把已经成功的 `backend-v1.2.3` 判为已发送；需要重新发布时，应在分发服务中明确执行补发，而不是直接删除去重记录。

## 自托管 n8n

仓库提供了可直接导入的工作流模板 [`ops/n8n/msime-release-social.json`](../ops/n8n/msime-release-social.json)。在 n8n 中选择 Import from File 导入后，先保持工作流未激活，配置环境变量，再用 Test URL 验证；确认每个分支都收到测试消息后才激活 Production Webhook。

n8n 进程需要能够访问公网，并设置 `N8N_BLOCK_ENV_ACCESS_IN_NODE=false`，让验签和 HTTP Request 表达式读取下面的变量。不要把这些值写进工作流 JSON：

| 变量 | 作用 |
| --- | --- |
| `MSIME_SOCIAL_WEBHOOK_SECRET` | 与 GitHub Secret `SOCIAL_WEBHOOK_SECRET` 相同 |
| `MSIME_TELEGRAM_ENABLED`、`MSIME_TELEGRAM_BOT_TOKEN`、`MSIME_TELEGRAM_CHAT_ID` | Telegram 开关、Bot Token 和会话 ID |
| `MSIME_DISCORD_ENABLED`、`MSIME_DISCORD_WEBHOOK_URL` | Discord 开关和 Webhook 地址 |
| `MSIME_MASTODON_ENABLED`、`MSIME_MASTODON_INSTANCE`、`MSIME_MASTODON_ACCESS_TOKEN` | Mastodon 开关、实例地址和 `write:statuses` Token |
| `MSIME_BLUESKY_ENABLED`、`MSIME_BLUESKY_IDENTIFIER`、`MSIME_BLUESKY_APP_PASSWORD` | Bluesky 开关、账号和 App Password |
| `MSIME_X_ENABLED`、`MSIME_X_ACCESS_TOKEN` | X 开关和具有发帖权限的 OAuth 用户 Token |
| `MSIME_REDDIT_ENABLED`、`MSIME_REDDIT_ACCESS_TOKEN`、`MSIME_REDDIT_SUBREDDIT`、`MSIME_REDDIT_USER_AGENT` | Reddit 开关、带 `submit` 权限的 OAuth Token、目标 subreddit 和描述性 User-Agent |

开关取值为 `1` 才会发帖。未配置的分支不会运行，单个平台失败也不会阻止其他分支。Reddit Token 和 X Token 可能过期，建议后续增加独立的 OAuth 刷新工作流；不要把长期凭据放进 GitHub Actions。

工作流使用 Webhook 的 Raw Body 验证 `X-MSIME-Signature`，再生成不同长度的文案：Telegram 4096 字符、Discord 2000 字符、Mastodon 500 字符、Bluesky 300 字符、X 280 字符。Reddit 发布链接帖，正文在 Release 页面保留。

每个平台的 Release 链接会带独立的 UTM 参数：`utm_source=<channel>`、`utm_medium=social`、`utm_campaign=<idempotency_key>`。官网或下载页的分析工具应保留这些参数，并按 `utm_source` 比较点击、安装和后续 star；不要只比较各平台的发帖成功数。

### 测试请求

先在 n8n 的 Test URL 上执行工作流，再用一个合成 Release 事件测试。签名必须对原始 JSON 正文计算，不能先格式化或重新排序：

```sh
payload='{"schema_version":1,"event":"release.published","idempotency_key":"test:backend-v0.0.0","repository":"test/repo","release":{"tag":"backend-v0.0.0","name":"测试版本","body":"- 合成变更","url":"https://example.test/release","published_at":"2026-01-01T00:00:00Z","target_commitish":"test"}}'
signature="sha256=$(printf %s "$payload" | openssl dgst -sha256 -hmac "$MSIME_SOCIAL_WEBHOOK_SECRET" | awk '{print $2}')"
curl --fail-with-body -X POST "$N8N_TEST_WEBHOOK_URL" \
  -H 'Content-Type: application/json' \
  -H "X-MSIME-Signature: $signature" \
  --data-binary "$payload"
```

模板默认关闭所有平台分支，测试时只打开 Telegram 或 Discord 其中一个，确认内容、长度和链接正确后再逐个启用。n8n 的成功执行数据被关闭保存，避免把 Token 或社交正文长期写入执行记录；发生错误时只看状态码和节点名，不要把上游响应正文复制到 issue 或日志。

工作流用 n8n 的全局静态数据记录每个平台已经成功的 `idempotency_key`，重跑时只补发失败的平台。这个记录只适合单进程或普通主从部署；n8n 队列模式下静态数据可能无法在执行器之间可靠共享，应把去重改成 Data Table 或 PostgreSQL，并让所有执行器使用同一个存储。静态数据会随工作流执行成功后保存，第一次在编辑器里手动测试时不会建立去重记录。

自托管 n8n 应启用 HTTPS、固定 `N8N_ENCRYPTION_KEY`、关闭公开注册，并限制能够编辑 Code 节点和读取环境变量的账号。升级 n8n 后先在测试 URL 验证 Raw Body、HMAC 和各平台分支，再重新激活 Production Webhook。

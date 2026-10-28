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
| `MSIME_SOCIAL_SITE_URL` | 官网推广入口，默认 `https://msime.app/download/`；必须为无账号、查询参数或锚点的 HTTPS 地址 |
| `MSIME_TELEGRAM_ENABLED`、`MSIME_TELEGRAM_BOT_TOKEN`、`MSIME_TELEGRAM_CHAT_ID` | Telegram 开关、Bot Token 和会话 ID |
| `MSIME_DISCORD_ENABLED`、`MSIME_DISCORD_WEBHOOK_URL` | Discord 开关和 Webhook 地址 |
| `MSIME_MASTODON_ENABLED`、`MSIME_MASTODON_INSTANCE`、`MSIME_MASTODON_ACCESS_TOKEN` | Mastodon 开关、实例地址和 `write:statuses` Token |
| `MSIME_BLUESKY_ENABLED`、`MSIME_BLUESKY_IDENTIFIER`、`MSIME_BLUESKY_APP_PASSWORD` | Bluesky 开关、账号和 App Password |
| `MSIME_X_ENABLED` | X 开关；发帖使用 n8n 中已授权的 `MSIME X OAuth2` 凭据 |
| `MSIME_REDDIT_ENABLED`、`MSIME_REDDIT_ACCESS_TOKEN`、`MSIME_REDDIT_SUBREDDIT`、`MSIME_REDDIT_USER_AGENT` | Reddit 开关、带 `submit` 权限的 OAuth Token、目标 subreddit 和描述性 User-Agent |

开关取值为 `1` 才会发帖。未配置的分支不会运行，单个平台失败也不会阻止其他分支。X 使用 n8n OAuth2 凭据自动刷新，Reddit 仍直接使用环境变量中的用户 Token，启用持续发布前需补齐它的刷新接线。不要把长期凭据放进 GitHub Actions。

X 应用从[开发者控制台](https://console.x.com/)申请。使用 OAuth 2.0 用户授权申请 `tweet.read`、`tweet.write`、`users.read` 和 `offline.access`，保存 Client ID 与 Client Secret，后续再完成账号授权和刷新令牌接线；App-only Bearer Token 不能用于发帖。授权流程与有效期见 [X 官方说明](https://docs.x.com/fundamentals/authentication/oauth-2-0/authorization-code)，发帖费用以[当前官方价格](https://docs.x.com/x-api/getting-started/pricing)为准。模板的「发送 X」节点使用 Generic Credential Type → OAuth2 API，引用 ID 为 `msimeXOAuth2`、名称为 `MSIME X OAuth2` 的凭据。在 n8n 创建该凭据时，Grant Type 选 PKCE，Authorization URL 为 `https://x.com/i/oauth2/authorize`，Access Token URL 为 `https://api.x.com/2/oauth2/token`，Authentication 选 Header，Scope 为上述四项；点击 Connect my account 完成用户授权。仅保存 Client ID/Secret 不代表已授权。HTTP Request 节点通过 n8n 管理用户 Access Token 与 Refresh Token，不再读取 `MSIME_X_ACCESS_TOKEN`。其他实例导入时应在该节点重新选择自己的凭据，不要照搬生产凭据数据。

Reddit 的外部 n8n 集成需提交 [Data API 访问申请](https://support.reddithelp.com/hc/en-us/requests/new?ticket_form_id=14868593862164)并获得审批。用途应如实描述为低频的项目版本公告，列出账号、目标 subreddit、项目链接及使用外部 n8n 的原因；Devvit 登录不会直接提供外部 API 凭据，见 [Reddit 官方 FAQ](https://developers.reddit.com/docs/guides/faq)。获批后再配置 OAuth 应用、`submit` 权限及令牌刷新，且只发布到允许此类公告的社区。Client Secret 和用户令牌存入 Vault，不要粘贴到 PR 或聊天中。

工作流使用 Webhook 的 Raw Body 验证 `X-MSIME-Signature`，再生成不同长度的文案：Telegram 4096 字符、Discord 2000 字符、Mastodon 500 字符、Bluesky 300 字符、X 280 字符。Reddit 发布链接帖，正文在 Release 页面保留。

每个平台的主推广链接指向官网，带独立的 `utm_source=<channel>`、`utm_medium=social` 和 `utm_campaign`。活动名称由版本标签与幂等键的 SHA-256 短摘要组成，只使用官网采集白名单允许的字符；完整幂等键仍用于去重，不变更历史发布记录。Telegram、Discord 和 Reddit 的长文另保留原始 Release 更新说明链接。文案先为完整链接预留长度，再截断更新内容，避免长说明切断推广入口。直接跳到 GitHub 的访问无法计入官网 Umami，不能据此比较官网转化。

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

## 每周效果周报

仓库还提供了 [`ops/n8n/msime-social-report.json`](../ops/n8n/msime-social-report.json) 周报模板。它每周运行一次，查询自托管 Umami 的 `social` UTM 访问、`Download` 下载点击和可选安装事件，查询 GitHub 仓库的 Star、Fork 与最近 Release，然后生成中文周报并按开关发送到内部 Telegram 或 Discord。Umami 是可选的；关闭时仍会生成 GitHub 增长和 Release 周报，只跳过官网流量查询。工作流默认未激活，导入后先手动执行一次，确认统计周期、平台名称、下载点击数量和安装接入状态都正确，再启用它。

周报使用以下 n8n 环境变量；令牌只放环境变量或 n8n Credentials，不要写入工作流 JSON：

| 变量 | 作用 |
| --- | --- |
| `MSIME_UMAMI_ENABLED` | Umami 查询开关，设为 `1` 才查询；完成 Website 和 API key 配置前保持关闭 |
| `MSIME_UMAMI_BASE_URL` | 自托管 Umami 地址，例如 `http://umami.umami.svc.cluster.local:3000` |
| `MSIME_UMAMI_API_KEY` | Umami Settings → API keys 创建的 Bearer API key |
| `MSIME_UMAMI_WEBSITE_ID` | Umami Website 的 ID |
| `MSIME_UMAMI_INSTALL_ENABLED` | 真实安装信号已接入后设为 `1`，默认关闭；关闭时显示“安装数据尚未接入” |
| `MSIME_UMAMI_INSTALL_EVENT` | 真实安装自定义事件名称，默认 `Install`；不应配置成 `Download` |
| `MSIME_GITHUB_REPOSITORY` | GitHub 仓库，例如 `metasequoiaime/msime-cloud` |
| `MSIME_GITHUB_TOKEN` | 读取仓库和 Release 的 GitHub Token，只需 `Metadata: read` |
| `MSIME_REPORT_TELEGRAM_ENABLED`、`MSIME_REPORT_TELEGRAM_BOT_TOKEN`、`MSIME_REPORT_TELEGRAM_CHAT_ID` | Telegram 周报开关、Bot Token 和内部群组 ID |
| `MSIME_REPORT_DISCORD_ENABLED`、`MSIME_REPORT_DISCORD_WEBHOOK_URL` | Discord 周报开关和内部 Webhook 地址 |

Umami Website 需要保留 `utm_source`、`utm_medium`、`utm_campaign`。官网已通过 `Download` 记录下载按钮点击，它不代表文件下载完成或应用安装；真实客户端安装信号尚未接入，默认不查询 `Install`，也不显示“零安装”。完成可靠安装信号接入后，才打开 `MSIME_UMAMI_INSTALL_ENABLED`。不要把下载点击改名为安装事件。

工作流通过 Umami `/api/websites/{id}/breakdown` 按 `utmSource` 分组，`fields` 使用 URL 编码后的 JSON 数组，过滤 `utmMedium=social`。只按来源分组可以避免把跨版本活动的同一访客重复累加。随后对本周期出现过访问的每个来源调用 `/api/websites/{id}/events/stats`，按 `event=Download`、`utmSource` 和 `utmMedium=social` 查询下载点击；安装启用时另查配置的安装事件。`breakdown` 只统计页面浏览，不能用于统计自定义事件。查询失败或响应缺少事件计数时标记数据不可用，不伪装成零点击。`utmCampaign` 保留在 Umami 原始记录中，可以在管理界面下钻具体版本。没有访问来源时查询全体社交事件，有事件则显示为 `unknown`；本周期仅有事件、没有页面浏览的来源可能不在分平台结果中。

GitHub Star 和 Fork 只能得到仓库总量，工作流用 n8n 静态数据保存上一次成功查询的快照，变化标为“较上次快照”，不能归因到单个平台，也不保证间隔恰好七天。第一次运行显示“首次记录”，GitHub 查询失败则显示不可用，不生成零 Star 或虚假的负增长。Release 列表使用精确时间戳过滤最近七天，不把起始日之前的发布算进本周期。

统计周期按 n8n 所在机器的 UTC 时间计算最近七天。工作流只保留上一份 Star/Fork 快照，不保存 Umami 明细或 GitHub 响应；HTTP 查询保留完整响应，使空数组仍能继续执行，Release 数组也不会被拆成多份重复周报。查询失败会在周报末尾标记数据不可用。发送节点开启失败继续执行，因此 Telegram 故障不会阻止 Discord 周报。队列模式下应确认 n8n 静态数据由同一个主进程持久化，否则请把快照迁移到 Data Table 或 PostgreSQL。

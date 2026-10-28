# 发版社交分发

生产入口是自托管 n8n 的客户端 Release 定时检查，不依赖 GitHub Actions 访问集群内网。`ops/n8n/msime-release-watch.json` 每五分钟读取 `metasequoiaime/msime` 和 `metasequoiaime/msime-windows` 的 GitHub Releases，通过同一 Pod 的 `http://127.0.0.1:5678/webhook/msime-release` 向 `ops/n8n/msime-release-social.json` 发送 HMAC 签名事件，再分发到已启用的社交平台。

`n8n.yldm.tech` 的 DNS 指向内网地址，编辑器和 webhook 不向公网开放。GitHub 托管 runner 无法连接此地址；不要再把它填成 Actions 的分发目标。后端发版工作流已移除 `announce` job，后端版本不触发宣传，旧的 `SOCIAL_WEBHOOK_URL`、`SOCIAL_WEBHOOK_SECRET` Actions Secrets 不再被默认发布流程使用。`scripts/release/social.py` 保留为有内网连通性时的手动发送工具。

## 宣传范围与重试

只分发 Windows（含 Windows 7）、macOS、Linux、Android、iOS、HarmonyOS 的正式版。主仓标签必须是 `windows-vX.Y.Z`、`windows7-vX.Y.Z`、`macos-vX.Y.Z`、`linux-vX.Y.Z`、`android-vX.Y.Z`、`ios-vX.Y.Z`、`harmony-vX.Y.Z`，Windows 独立仓使用 `vX.Y.Z`。草稿、预发布、nightly、alpha、beta、后端和其他仓库的事件都会被过滤；分发工作流再次执行同样的范围检查。

检查任务仅接受启用时间之后、最近 24 小时内发布的版本，并等待至少一个附件、全部附件上传完成且大小大于零。生产导入时固定静态数据 `enabledAt`，以后更新工作流必须保留它；首次导入其他实例且未设这个值时，第一次检查自动记录当前时间，不补发历史版本。停机超过一天的版本需管理员明确补发。

在此时间范围内，每轮检查都以 `repository:tag` 重送事件。分发工作流按版本、平台记录成功，因此已成功的平台跳过、失败的平台下轮重试。请求按批次串行发送，webhook 等本次执行结束再返回，避免同一轮并发覆盖 n8n 的静态去重状态。发帖失败只记录平台名、时间和状态码到 `failures`，成功后清理对应失败标记，不保存平台响应或令牌。检查任务的 `sourceErrors` 和 `lastDispatch` 分别保留仓库读取与内部投递错误。

这是单实例 n8n 的部署约定；升级为队列或多执行器前，必须把去重和任务租约迁移到共享 PostgreSQL，不能只复制工作流。

## 上线顺序

1. 配置固定 `N8N_ENCRYPTION_KEY`，启用 `N8N_BLOCK_ENV_ACCESS_IN_NODE=false`，只给可信管理员工作流编辑权限。
2. 配置下表的 Vault/环境变量与已授权的 n8n X OAuth2 凭据。
3. 按原 ID `msimeReleaseSocial` 导入、发布分发工作流，保留已有静态数据与 X 凭据引用；等待 Pod 重启加载。
4. 导入检查工作流为 `msimeReleaseWatch`，设置 `staticData.global.enabledAt` 为启用时间。先运行不发帖验收，确认两个 GitHub 查询成功、签名请求确实到达本地 webhook。
5. 发布检查工作流并重启 n8n，确认其已发布版本、每五分钟的实际执行和新的 `lastCheckAt`。不需要为客户端仓库创建新的 Actions Secrets 或 webhook。

| 环境变量 | 用途 |
| --- | --- |
| `MSIME_GITHUB_TOKEN` | 读取两个公开客户端仓库的 Release，复用周报已有 GitHub 凭据 |
| `MSIME_SOCIAL_WEBHOOK_SECRET` | 检查与分发工作流共享的 HMAC 密钥，来自 Vault `secret/yldm/production/msime-social` 的 `webhook_secret` |
| `MSIME_SOCIAL_SITE_URL` | 官网入口，默认 `https://msime.app/download/`，必须为无账号、查询参数和锚点的 HTTPS URL |
| `MSIME_X_ENABLED` | 设为 `1` 启用，使用 n8n 已授权的 `MSIME X OAuth2` 凭据（ID `msimeXOAuth2`）自动刷新 Token |
| `MSIME_BLUESKY_ENABLED`、`MSIME_BLUESKY_IDENTIFIER`、`MSIME_BLUESKY_APP_PASSWORD` | Bluesky 开关、账号和应用密码 |
| `MSIME_MASTODON_ENABLED`、`MSIME_MASTODON_INSTANCE`、`MSIME_MASTODON_ACCESS_TOKEN` | Mastodon 开关、账号所在实例的 HTTPS URL 和 `write:statuses` 用户 Token |
| `MSIME_TELEGRAM_ENABLED`、`MSIME_TELEGRAM_BOT_TOKEN`、`MSIME_TELEGRAM_CHAT_ID` | 可选 Telegram 分发；生产保持关闭，客户端公告已由后端通过 `@msime_bot` 投递到用户群，避免重复公告 |
| `MSIME_DISCORD_ENABLED`、`MSIME_DISCORD_WEBHOOK_URL` | 可选 Discord 分发 |
| `MSIME_REDDIT_ENABLED`、`MSIME_REDDIT_ACCESS_TOKEN`、`MSIME_REDDIT_SUBREDDIT`、`MSIME_REDDIT_USER_AGENT` | 可选 Reddit 分发，需先完成 Data API 审批和令牌刷新接线，当前未启用 |

`MSIME_RELEASE_WATCH_DRY_RUN=1` 只用于隔离验收进程：检查任务在签名的正文中添加 `dry_run: true`，分发器验签后返回 `probe` 项并更新 `lastVerification`，不触发发帖、不记录成功。生产容器不得设置此变量，否则正式版本也只会验收。

## Webhook 合约与验收

内部请求使用 `POST`，正文为 UTF-8 JSON，`X-MSIME-Signature: sha256=<HMAC-SHA256>` 对原始字节计算。正文包含 `schema_version: 1`、`event: release.published`、`repository`、`idempotency_key: repository:tag` 与 `release`（`tag`、`name`、`body`、`url`、`published_at`、`target_commitish`）。Webhook 的 Raw Body 位于 `binary.data`，不能重新序列化后再验签。

不发帖验收使用合成的正式版标签，如 `android-v0.0.0`，在签名前加入 `dry_run: true`。确认返回的 `probe` 列出 X、Bluesky、Mastodon，且已发布工作流的 `lastVerification` 与请求一致；错误签名必须失败。验收不能删除真实版本的去重记录，也不补发历史 Release。

正式发版后，检查 `msimeReleaseWatch` 的 `lastCheckAt`、`lastDispatch`、`sourceErrors`，以及 `msimeReleaseSocial` 的 `completed` 和 `failures`。HTTP 成功只表示本次内部执行结束，具体平台是否发布以平台成功记录及帖子为准。

## 平台授权与链接

Bluesky 应用密码入口：[App passwords](https://bsky.app/settings/app-passwords)。模板使用 `bsky.social`，不使用账号主密码。完整会话响应从 `body` 读取 JWT/DID；授权失败不会发帖。推广链接用 UTF-8 字节范围的 facet 标记为可点击链接。

Mastodon 在账号所在实例 Settings → Development 创建 `write:statuses` 用户 Token，Redirect URI 可用 `urn:ietf:wg:oauth:2.0:oob`。发帖携带 `Idempotency-Key` 减少短期重复；长期去重仍由工作流负责。Bluesky、Mastodon 只在响应包含帖子 `uri` 或 `id` 时记录成功。

X 使用 OAuth2 PKCE 的 `tweet.read tweet.write users.read offline.access`，通过 n8n 的 `MSIME X OAuth2` 完成账号授权和刷新，不能只保存 Client ID/Secret，也不能使用 App-only Token 发帖。替换账号或应用时在 n8n 重新授权；其他实例导入时重新选择自己的凭据。

主推广链接包含 `utm_source=<channel>`、`utm_medium=social`、由标签与幂等键摘要构成的 `utm_campaign`；完整链接先预留长度，再截断文案。Telegram、Discord 和 Reddit 长文另含原始 Release 链接。直接进入 GitHub 的流量无法计入官网 Umami。

Bluesky、Mastodon 字段由 yldm-platform 的 ExternalSecret 从 `secret/yldm/production/msime-social` 注入，开关为 `1` 才发帖；X 用户令牌加密存放于 n8n 数据库。DEV.to 尚未接入。不要将凭据或上游响应写进 Git、PR、聊天或执行日志；成功与失败执行详情均关闭保存。

## 每周效果周报

仓库还提供了 [`ops/n8n/msime-social-report.json`](../ops/n8n/msime-social-report.json) 周报模板。它按 `Asia/Shanghai` 时区在每周一 09:00 运行一次，查询自托管 Umami 的 `social` UTM 访问、`Download` 下载点击和可选安装事件，查询 GitHub 仓库的 Star、Fork 与最近 Release，然后生成中文周报并按开关发送到内部 Telegram 或 Discord。Umami 是可选的；关闭时仍会生成 GitHub 增长和 Release 周报，只跳过官网流量查询。工作流默认未激活，导入后先手动执行一次，确认统计周期、平台名称、下载点击数量和安装接入状态都正确，再启用它。

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

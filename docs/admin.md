# 管理后台

管理后台是挂在独立域名（默认 `admin.msime.app`）上的单页应用，源码在 `admin-web/`。技术栈：React 19、TypeScript、Vite 8、Tailwind CSS v4、TanStack Router / Query / Table、Radix UI、Recharts、Zod、react-hook-form、cmdk、date-fns、lucide-react、pnpm 10.15 和 Biome，不再使用 Sass。Vite 生成 `admin-web/dist/`，`admin-web/embed.go` 把产物嵌入 Go 二进制；Docker 在 Node 构建阶段重新构建前端，再编译进 Go 镜像。后台与现有 HTTP 服务共用端口，不需要单独启动 Node、前端容器或静态文件服务器。

后台页面受严格 CSP 约束：`default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'`。不允许 eval、内联样式表和外链图片，所以不显示 Google 头像，Zod 以 jitless 模式运行，也不使用会注入 `<style>` 的 Radix Overlay。

## 启用与部署

1. 启用现有 PostgreSQL 用户体系（`auth.enabled: true`），设置 `MSIME_DATABASE_URL` 与 `MSIME_AUTH_PEPPER`。数据库最低版本为 PostgreSQL 12，见下文「PostgreSQL 兼容性」。
2. 在 Google Cloud 项目中创建 Web OAuth 客户端，授权重定向 URI 设为 `https://admin.msime.app/api/auth/google/callback`。登录范围是 `openid email profile`，`profile` 只用来在外壳和个人中心显示管理员的 Google 名字。把 Client Secret 保存到 `MSIME_ADMIN_GOOGLE_SECRET`，把所有者邮箱白名单保存到 `MSIME_ADMIN_GOOGLE_EMAILS`（逗号分隔）。
3. 在配置中加入 `admin` 块。下面是包含全部配置项的示例，各块的含义见「配置」一节：

   ```json
   "admin": {
     "enabled": true,
     "host": "admin.msime.app",
     "token_env": "MSIME_ADMIN_TOKEN",
     "google": {
       "client_id": "YOUR_WEB_CLIENT_ID.apps.googleusercontent.com",
       "secret_env": "MSIME_ADMIN_GOOGLE_SECRET",
       "redirect_uri": "https://admin.msime.app/api/auth/google/callback",
       "allowed_emails_env": "MSIME_ADMIN_GOOGLE_EMAILS"
     },
     "environment": "生产环境",
     "github": {
       "app_id": 123456,
       "installation_id": 7890123,
       "private_key_env": "MSIME_ADMIN_GITHUB_APP_KEY",
       "dictionary_repo": "metasequoiaime/msime-dictionary",
       "issue_repos": ["metasequoiaime/msime", "metasequoiaime/msime-windows"],
       "platforms": [
         {"id": "windows", "name": "Windows", "repo": "metasequoiaime/msime-windows", "tag_prefix": "windows-v", "release_workflow": "release.yml", "assignee": "houko", "label": "windows"}
       ]
     },
     "services": [
       {"key": "translation", "name": "翻译", "provider": "DeepL", "quota": {"limit": 500000, "unit": "chars", "period": "month", "unit_price": 0}, "slow_ms": 3000}
     ],
     "telegram": {
       "bot_token_env": "MSIME_ADMIN_TELEGRAM_TOKEN",
       "chat_id": "@msime_news"
     },
     "logs": {
       "loki_url": "http://loki.loki.svc.cluster.local:3100",
       "selector": "{namespace=\"app\",container=\"msime-backend\"}"
     }
   }
   ```

4. Google 登录模式不需要 `MSIME_ADMIN_TOKEN`。保留它时，登录页额外提供「管理员密钥登录」作为兼容入口；不配置 Google 时仍需要至少 32 字节的独立随机管理员密钥。管理员密钥不能以 `msime_pat_` 开头，这个前缀留给个人访问令牌，配置校验会拒绝。不要把任何密钥放进前端源码、安装包或版本库。
5. 运行账号有 DDL 权限时不需要单独迁移：启动时发现缺少后台表或列，会自动执行 `internal/account/admin_schema.sql` 和 `internal/account/admin_ops_schema.sql`，两者都是幂等的追加式迁移。运行账号按最小权限只有 DML 时，先用有 DDL 权限的账号执行 `./msime-server -config /config/config.json -migrate-users`，再给运行角色授予新表的 `SELECT, INSERT, UPDATE, DELETE` 以及 bigserial 序列的 `USAGE, SELECT`。新表包括 `admin_roles`、`admin_role_permissions`、`admin_tokens`、`admin_notifications`、`admin_notification_reads`、`admin_preferences`、`admin_notices`、`admin_crash_groups`、`admin_service_metrics`、`admin_service_daily`、`admin_service_minutes`、`admin_service_verdicts`、`admin_incidents`、`admin_sensitive_words`、`admin_sensitive_hits`、`release_asset_snapshots`、`community_reports`、`word_submissions` 和 `site_settings`；已有的 `admin_members`、`admin_sessions`、`admin_audit`、`admin_events` 和四张社区表新增了列。`admin_role_permissions` 的权限 CHECK 约束随服务日志权限 `view_logs` 一起更新（同时给维护者授予一次 `view_logs`），这同样需要 DDL 权限。后台启用而表、列或约束既不存在又补不上时，服务拒绝启动并在错误里说明原因。
6. 正常启动镜像，容器中的 `listen` 应为 `0.0.0.0:8080`。把 `admin.msime.app` 的 DNS 指向入口，在入口终止 HTTPS，把该域名的请求转发到相同的 Go 端口，并保留原始 Host。Go 不信任 `X-Forwarded-Host`。

示例 Nginx HTTPS 虚拟主机（证书路径、后端地址按部署调整）：

```nginx
server {
    listen 443 ssl;
    server_name admin.msime.app;
    ssl_certificate /etc/nginx/certs/admin.msime.app/fullchain.pem;
    ssl_certificate_key /etc/nginx/certs/admin.msime.app/privkey.pem;
    location / {
        proxy_set_header Host $host;
        proxy_pass http://msime-backend:8080;
    }
}
```

默认 `admin.enabled: false`，不会改变已有域名的路由。后台域名不承载 `/v1/*` 客户端 API。

本地私密配置可放在 `config.admin.local.json` 和 `.env.admin.local`（均被 Git 忽略）。Go 不自动加载 `.env`，可由部署工具注入，或在本机先 `set -a; . ./.env.admin.local; set +a`。

### PostgreSQL 兼容性

后台的全部迁移和查询都兼容 PostgreSQL 12 及以上版本：

- 不使用 PostgreSQL 13 才进入核心的 `gen_random_uuid()`。`admin_sessions.id` 这类短随机句柄由 `left(md5(random()::text||clock_timestamp()::text),16)` 生成。
- 社区皮肤相关表用到生成列，要求 12 及以上，这也是整个服务的最低版本。
- 在 `ADD COLUMN IF NOT EXISTS` 里写 `REFERENCES` 并非在所有版本上都受 `IF NOT EXISTS` 保护，所以外键（如 `admin_members.role → admin_roles`）放在 `DO` 块里，先查 `pg_constraint` 再单独添加；放宽 `admin_events.kind` 的 CHECK 约束也是同样的写法。
- 迁移可以重复执行。内置角色的默认权限只在创建角色行的那条语句里写入，之后在后台改过的权限矩阵不会被重跑的迁移覆盖。

## 登录、身份与令牌

后台有三种身份。每个 `/api/*` 请求都会重新解析身份和权限，所以改角色、停用成员、从白名单删除所有者都即时生效。

| 身份 | 认证方式 | 审计中的 actor | 角色与权限 |
| --- | --- | --- | --- |
| 所有者 | Google 登录，邮箱在 `google.allowed_emails` / `allowed_emails_env` 中 | `google:<sub>:<email>` | 恒为维护者，拥有全部 8 项权限，不受权限矩阵影响 |
| 成员 | Google 登录（浏览器或命令行）或个人访问令牌，邮箱在已启用的 `admin_members` 中 | `google:<sub>:<email>` 或 `pat:<email>` | `admin_members.role` 对应角色在权限矩阵中的权限 |
| 管理员密钥 | `Authorization: Bearer <MSIME_ADMIN_TOKEN>` | `legacy-token` | 维护者，拥有除 `manage_permissions` 以外的全部权限；没有个人中心 |

Google 登录时，后端校验 ID Token 的签名、issuer、audience、有效期、nonce、`email_verified`，以及邮箱是否在白名单或 `admin_members` 中。授权码流程使用 PKCE S256 和一次性 state；state 与浏览器 HttpOnly Cookie 绑定，在数据库中保留 10 分钟。管理员会话在 PostgreSQL 中只存令牌哈希，有效期固定 8 小时；浏览器使用 Secure、HttpOnly、SameSite=Lax、无 Domain 的 `__Host-` Cookie。会话另外记录创建时间、最近活动时间（每 5 分钟最多更新一次）、截断到 256 字符的 User-Agent 和 Google 名字，供个人中心显示和吊销。Cookie 会话发起的写请求要求 `Origin` 与回调地址的来源完全相同，否则返回 403 `origin_required`；后台的访问地址（协议和域名）与 `admin.google.redirect_uri` 不一致时，所有写操作都会因此失败。普通 Google 用户不会因此成为管理员，也不会自动创建输入法用户账户。Google Cloud 项目处于测试发布状态时，需要把管理员加入测试用户。此处遵循 [Google OpenID Connect 服务端流程](https://developers.google.com/identity/openid-connect/openid-connect)。

| 端点 | 用途 |
| --- | --- |
| `GET /api/auth/session` | 登录状态、版本号、管理员邮箱和启用的登录方式 |
| `GET /api/auth/google/start` | 创建 state、nonce 和 PKCE，跳转到 Google |
| `GET /api/auth/google/callback` | 校验回调并创建管理员会话 |
| `POST /api/auth/logout` | 删除服务端会话（Cookie 或 Bearer 携带的会话）并清除 Cookie |
| `POST /api/auth/cli/start` | 命令行登录：`{"redirect_uri":"http://127.0.0.1:<端口>/callback"}`，返回 `state`、`expires_in` 和 Google 授权地址 |
| `POST /api/auth/cli/finish` | 命令行登录：`{"state","code","redirect_uri"}`，校验后返回 8 小时的管理员会话 `token` |

### 命令行登录

`msime-cloud login admin` 让所有者和成员在终端里用自己的 Google 账号登录后台，之后 `msime-cloud call GET /api/...` 自动带上会话，不需要管理员密钥，也不需要先在浏览器里生成个人访问令牌。

- 网页客户端只能回调到后台域名，所以命令行登录使用用户体系已有的「桌面应用」OAuth 客户端（`auth.google.desktop`），按 RFC 8252 回调到本机 `127.0.0.1` 的临时端口。需要同时配置 `admin.google` 和 `auth.google.desktop`，两者缺一时 `cli/*` 返回 404 `cli_login_disabled`，`GET /api/auth/session` 的 `cli_enabled` 为 false。桌面客户端无需登记回调地址；Google Cloud 项目处于测试发布状态时，同样要把管理员加入测试用户。
- 服务端持有桌面客户端密钥和 PKCE verifier，自己用授权码换取 ID Token，按网页登录完全相同的规则校验（签名、issuer、audience 为桌面客户端、有效期、nonce、`email_verified`、白名单或 `admin_members`），并同样记录 Google 名字和 User-Agent。state 不绑定 Cookie，而是由命令行在本机回调上核对；回调地址不入库，换取令牌时 Google 会拒绝与授权时不同的 `redirect_uri`。`cli/start` 与网页登录共用每个来源 IP 每分钟 10 次的限额。
- 返回的会话与浏览器会话是同一种：8 小时、只存哈希、出现在个人中心的会话列表里、可吊销，审计 actor 为 `google:<sub>:<email>`。命令行以 `Authorization: Bearer <token>` 发送，和个人访问令牌一样不需要 `Origin`；`POST /api/auth/logout` 带上它即结束该会话。

### 个人访问令牌（PAT）

所有者和成员可以在个人中心生成个人访问令牌，用脚本调用后台 API：

- 格式为 `msime_pat_` 加 64 位十六进制，共 74 个字符。完整令牌只在生成时返回一次，数据库只存哈希和末 4 位。
- 有效期 30 天。重新生成会在同一事务里删除该邮箱的旧令牌，所以每人同时只有一个有效令牌。
- 用法：向后台域名发送 `Authorization: Bearer msime_pat_…`。令牌请求不需要 `Origin`，因为浏览器不会自动携带 Bearer 头，不存在跨站风险。
- 令牌没有独立的权限：每次请求都按该邮箱当前是否为所有者、以及 `admin_members` 记录重新判定，成员被停用后令牌立即失效。审计 actor 记为 `pat:<email>`，限流也按这个 actor 单独计。
- 用令牌访问个人中心时（`GET /api/me` 返回 `via: "token"`），资料和「安全」卡片标明为个人访问令牌访问，不显示 Google 两步验证状态；令牌不能为自己续期，重新生成只能在浏览器登录会话中操作。
- 管理员密钥身份没有个人中心，不能生成令牌。

## 角色与权限

权限共 9 项，与「权限日志」页的矩阵一一对应：

| 权限键 | 含义 |
| --- | --- |
| `review_dict_pr` | 词库 PR 的精简、通过和驳回 |
| `review_community` | 社区内容的通过、下架、恢复和删除；敏感词库的增删改 |
| `triage_issues` | Issue 分诊与回复；崩溃分组状态和为崩溃分组建 Issue；旧崩溃列表的处理；故障事件的开启、更新和恢复 |
| `ban_users` | 封禁、解封用户，吊销用户会话 |
| `publish_notices` | 发布、归档公告；修改官网下载镜像链接（`POST /api/site-settings`） |
| `trigger_release` | 触发发布流水线、编辑发布说明、撤回版本 |
| `view_cloud_usage` | 读取云端监控（`GET /api/cloud`） |
| `view_logs` | 读取服务日志（`GET /api/logs`、`GET /api/logs/stream`） |
| `manage_permissions` | 修改权限矩阵 |

内置 4 个角色，默认矩阵如下（✓ 为拥有）：

| 权限 | 维护者 `maintainer` | 审核志愿者 `reviewer` | 运营/客服 `operator` | 只读 `readonly` |
| --- | --- | --- | --- | --- |
| `review_dict_pr` | ✓ | ✓ | | |
| `review_community` | ✓ | ✓ | | |
| `triage_issues` | ✓ | ✓ | ✓ | |
| `ban_users` | ✓ | | ✓ | |
| `publish_notices` | ✓ | | ✓ | |
| `trigger_release` | ✓ | | | |
| `view_cloud_usage` | ✓ | | ✓ | ✓ |
| `view_logs` | ✓ | | | |
| `manage_permissions` | ✓ | | | |

规则：

- 除云端监控（`view_cloud_usage`）和服务日志（`view_logs`）外，所有 GET 请求对所有角色开放，「只读」角色就是靠这一点成立的。缺少权限的写操作返回 403 `permission_denied`，前端对应按钮置灰并提示所需权限；云端监控和服务日志在侧栏和搜索中对没有对应权限的角色隐藏。
- `view_logs` 默认只有维护者拥有（所有者恒为维护者，旧版管理员密钥拥有除 `manage_permissions` 外的全部权限），需要时可以在权限矩阵里授予其他角色。已有部署升级时，迁移只在替换旧约束的那一次给维护者授予 `view_logs`，之后在后台收回不会被重跑的迁移补回。
- 维护者的 `manage_permissions` 不能收回（409 `protected`），迁移也会在它缺失时补回。权限矩阵通过 `POST /api/permissions {action: grant|revoke, role, permission}` 修改。
- 所有者恒为维护者，不能在后台修改、停用或撤销（403 `protected_owner`），恢复入口始终在部署配置里。
- 成员的增删、启停和改角色走 `GET/POST /api/admins`，只有所有者能调用。请求体为 `{"email","action":"add|enable|disable|revoke|set_role","role"?}`：`add` 可带 `role`（默认维护者），`set_role` 必须带 `role`。最多 100 个成员（409 `admin_limit`），重复添加返回 409 `admin_exists`。停用不删除记录，重新启用后需重新登录。添加、启用、停用和撤销都会删除该邮箱已有的后台会话和个人访问令牌，所以从配置中移除的前所有者被重新添加为成员时，旧凭据不会复活。成员记录不创建输入法用户账户，也不发送邀请邮件，被添加者直接用 Google 账号登录。
- 引入角色之前已有的成员在迁移时成为维护者，权限不变。
- `save_notice_draft` 不需要权限，任何角色都能写草稿，发布才需要 `publish_notices`。个人中心的偏好、会话和令牌操作也不需要权限，只作用于本人。

## 限流与通用约定

- 限流按身份分桶：`admin:<actor>`，每分钟 300 次，超限返回 429 `rate_limit_exceeded` 并带 `Retry-After: 60`。以前是所有管理员共享一个每分钟 120 次的桶；外壳每 60 秒轮询一次，多人同时在线会互相挤占，所以改为按人计。同一个人的多个 Google 会话共享一个桶，PAT 另算一个桶。
- 登录相关端点（`/api/auth/*`）仍按来源地址计（见「客户端地址」：配置了 `client_ip_header` 时取代理写入的地址，否则是 TCP 对端），每分钟 120 次；其中发起 Google 登录的 `/api/auth/google/start` 和命令行登录的 `/api/auth/cli/start` 共用每个来源地址每分钟 10 次的限额，超限同样返回 429 并带 `Retry-After: 60`。
- 以上三项限额在启用数据库（`auth.enabled`）时记在 PostgreSQL 的 `auth_rates` 表（作用域分别为 `admin`、`admin-auth`、`admin-login`，身份和 IP 只存摘要），为所有副本共享的固定一分钟窗口，所以多副本轮询负载均衡不会把限额放大成副本数倍；窗口边界前后最多可连续用掉两个窗口的额度。计数失败时返回 503 `admin_auth_unavailable` 并带 `Retry-After: 30`，不放行请求。未启用数据库时退回各进程内存中的令牌桶，限额按副本各自计算，只适合单副本。
- 每个后台请求有 15 秒的服务端超时。
- 数据库写操作统一经过 `POST /api/actions`，请求体为 `{"action","id"?,"user_id"?,"ids"?: [≤100],"reason"?: ≤500 字,"section"?,"value"?: JSON}`，拒绝未知字段。`value` 一般不超过 8 KiB，`save_notice_draft` 和 `publish_notice` 放宽到 256 KiB，以容纳 20000 字的公告正文。批量操作在单个事务内完成，审计与变更同事务写入，失败不部分生效。涉及 GitHub 的操作走各自的 REST 子路径，例如 `POST /api/dict-prs/{n}/approve`。
- 错误响应形如 `{"error":{"code","message"}}` 或 `{"error":"code"}`。前端的中文提示集中在 `admin-web/src/api/client.ts` 的 `codeMessages` 里，页面只在语境需要不同措辞时覆盖个别代码。
- 可逆操作（社区状态、Issue 标签、封禁、崩溃状态）完成后，提示条提供 4 秒「撤销」，调用服务端的反向操作。不可逆操作（合并、驳回词库 PR，为崩溃分组建 Issue）延迟 4 秒才真正发出，期间点「撤销」即取消。在此期间又做了一个可撤销或延迟的操作时，前一个立即发出；普通提示（包括前一个延迟操作的失败提示）显示在它上方，不会提前发出它，也不会遮住它的「撤销」。退出登录前会先发出并等它完成；关闭页面时立即以 keepalive 请求发出。

## 外壳

外壳由 `GET /api/shell` 一次性提供，每 60 秒刷新：版本号、环境标签（`admin.environment`）、本人邮箱、名字、角色和权限、侧栏待处理角标（词库 PR、社区待复核、待分诊 Issue）、未读通知数、后端状态（`ok`、`degraded`、`down`；前端也接受 `unknown` 并显示「状态未知」），以及部署配置启用的可选功能 `features`（目前只有 `logs`，即是否配置了 `admin.logs.loki_url`，未启用时侧栏不显示服务日志页）。

- 全局搜索（按 `/` 聚焦）：前端匹配页面名，`GET /api/search?q=` 最多返回 8 条，覆盖用户、社区内容、崩溃分组、敏感词、公告，以及内存缓存中的 GitHub PR、Issue 和 Release。结果通过 `?focus=<id>` 打开对应页面的详情。
- 通知：`GET /api/notifications?limit=20`，`POST /api/notifications/read {ids}|{all:true,up_to_id?}`。「全部已读」带上列表中最新一条通知的 `up_to_id`，只把创建时间不晚于它的通知标为已读，打开列表之后才到的通知仍是未读；不带 `up_to_id` 时标记到服务端当前时间。通知列表每次打开都重新加载；角标在弹层关闭时跟随外壳的 60 秒轮询，在个人中心切换通知偏好后立即刷新。来源包括新举报（与举报同事务写入）、新词库 PR、崩溃分组 7 天环比上升超过 20%（每小时检查，同一分组 7 天内只提醒一次）、自动开启的故障事件、Release 状态变化和新 Issue。个人中心可以按类型关闭词库 PR、举报和崩溃提醒。新加入的管理员会看到全部历史通知为未读。
- 外观：浅色、深色或跟随系统；配色可选春、夏、秋、冬或「自动」。自动配色按本地月份切换（3–5 月春，6–8 月夏，9–11 月秋，12–2 月冬），页面一直开着跨过月份边界时也会自动切换。外观保存在浏览器本地。
- 旧路径重定向：`/admins`、`/audit` → `/perm`，`/system` → `/status`，`/crashes` → `/crash`，`/skins`、`/dictionaries`、`/replies` → `/community?tab=skins|dictionaries|replies`，原有查询参数保留。

## 页面

| 页面 | 路径 | 数据来源 | 写操作所需权限 |
| --- | --- | --- | --- |
| 数据概览 | `/` | `GET /api/overview` | — |
| 词库审核 | `/dictpr` | `GET /api/dict-prs`、`/api/dict-prs/{n}`（GitHub） | `review_dict_pr` |
| 社区审核 | `/community` | `GET /api/{skins,candidate-skins,plugins,dictionaries,replies,phrases}`、`/api/community/counts` | `review_community` |
| 问题分诊 | `/issues` | `GET /api/issues`、`/api/issues/{owner}/{repo}/{n}`（GitHub） | `triage_issues` |
| 用户反馈 | `/feedback` | `GET /api/feedback`、`/api/feedback/{id}/screenshots/{n}` | `triage_issues` |
| 敏感词库 | `/words` | `GET /api/sensitive-words` | `review_community` |
| 用户账号 | `/users` | `GET /api/users`、`/api/users/stats`、`/api/users/{id}` | `ban_users` |
| 下载记录 | `/downloads` | `GET /api/downloads/summary`、`GET /api/site-settings` | 修改官网下载镜像 `publish_notices` |
| 公告推送 | `/notice` | `GET /api/notices` | 草稿无要求，发布与归档 `publish_notices` |
| 发布管理 | `/release` | `GET /api/releases`、`/api/releases/{platform}`（GitHub） | `trigger_release` |
| 云端监控 | `/cloud` | `GET /api/cloud` | 读取即需要 `view_cloud_usage` |
| 崩溃上报 | `/crash` | `GET /api/crash-groups`、`/api/crash-groups/{signature}` | `triage_issues` |
| 系统状态 | `/status` | `GET /api/status` | 故障事件需要 `triage_issues` |
| 服务日志 | `/logs` | `GET /api/logs/stream`（`GET /api/logs` 供命令行和脚本使用） | 读取即需要 `view_logs`；只在配置了 `admin.logs.loki_url` 时出现 |
| 权限日志 | `/perm` | `GET /api/permissions`、`/api/audit` | `manage_permissions`；成员管理仅限所有者 |
| 个人中心 | `/me` | `GET /api/me` | 只作用于本人 |

- **数据概览**：累计下载、注册用户、近 30 天新增、活跃设备（按天分 Windows、Mac/Linux、移动端）、近 7 天各平台活跃、无崩溃会话率及拖累最大的平台版本、最新出现的崩溃分组、待处理事项和服务状态。活跃与会话指标依赖客户端上报 `active`、`session`、`session_crash`，近 60 天没有任何上报时显示「客户端未上报」而不是 0。「累计下载」来自下载事件，不是安装数。
- **词库审核**：只列词库仓库自身 `community-words/` 分支上的 PR（忽略 fork），读最近 100 个。详情比较 PR 基准提交与头部提交的 `custom/{words,english,translations}.txt`，逐条标记：`ad` 命中敏感词，`bad` 不符合官网校验规则，`dup` 已在基准文件或内置词库中、或在 PR 内重复，`new` 可收录。可以只保留勾选项（逐个文件以 blob SHA 做比较交换后重写分支，并改写 PR 标题），可以通过（先精简，再以头部 SHA 为条件 squash 合并），也可以驳回（先评论「审核未通过：原因」再关闭）。三种操作都带上审核时看到的头部 SHA，PR 此后有了新提交（例如官网又追加了投稿）就返回 409 `pr_changed`，不会对没人看过的词条生效；精简时每个新提交的父提交也必须是上一个头部，否则说明分支在精简途中被追加了投稿，立即停止，不再继续写入或合并。精简和通过前会读取 PR 的改动文件列表，只要改动了这三个文件之外的任何文件就返回 409 `unexpected_files`，以免审核只看到词条而合并带进别的改动。页面显示每次投稿的补充说明和时间，来自 `word_submissions` 表。PR 作者是提交用的 GitHub App 时显示为「官网机器人」。
- **社区审核**：皮肤、候选皮肤、插件、词库、回复模板、短语包 6 个分类（短语包卡片显示条数和前三条正文，抽屉显示全部短语及分组），按待复核、已通过、已下架筛选。卡片显示自动检查标记和被举报次数；详情抽屉显示举报记录、实时敏感词检查、作者的其他作品和皮肤键盘预览，候选皮肤的预览图由 `GET /api/candidate-skins/{id}/preview` 提供。操作为 `approve_content`、`remove_content`（需要原因）、`restore_content`（恢复下架前的状态）和永久删除。候选皮肤另有图库分类：`GET /api/candidate-skins` 可用 `category=<分类>` 筛选，列表行和详情都带 `category`；详情抽屉的「分类」下拉框调用 `set_candidate_skin_category`（需要 `review_community`），请求体为 `{"action":"set_candidate_skin_category","id":"…"（或 "ids":[…] 最多 100 个）,"section":"candidate-skins"（可省略）,"value":{"category":"<分类>"}}`，分类取值见 [皮肤社区 · 分类](skin-community.md#分类)，未知分类 400 `invalid_category`，section 不是 `candidate-skins` 时 400 `invalid_section`，没有一个 id 存在时 404 `not_found`。修改不改变审核状态，也不更新 `updated_at`（不影响「通过并上架」固定的版本）；审计记录 `category`、`ids`、`count`，单项时还有 `name` 和原分类 `from`。键盘皮肤（`skins`）用同一组分类：`GET /api/skins` 可用 `category=<分类>` 筛选，列表行和详情都带 `category`，详情抽屉的「分类」下拉框调用 `set_skin_category`（需要 `review_community`），请求体为 `{"action":"set_skin_category","id":"…"（或 "ids":[…] 最多 100 个）,"section":"skins"（可省略）,"value":{"category":"<分类>"}}`，错误码、审计字段与 `set_candidate_skin_category` 相同，section 不是 `skins` 时 400 `invalid_section`；修改不改变审核状态。作者因封禁被下架的内容只能通过解封恢复（409 `owner_banned`）。审核规则见「社区事后审核」。
- **问题分诊**：遍历 `admin.github.issue_repos`，按平台 label 归类。状态映射：open 且无 `triaged` 标签为「新」，有 `triaged` 为「已分类」，closed 为「已关闭」，closed 且有 `duplicate` 标签为「重复」。可以分类（加 `triaged` 并指派给平台的 `assignee`）、标记重复、关闭、重新打开和回复。分类、标记重复和关闭完成后提示条提供「撤销」（分类的撤销即取消分类，平台的 `assignee` 在分类前已被指派的保留指派）；重新打开和回复没有撤销，需要时手动关闭或在 GitHub 上删除回复。每个仓库最多读 3 页共 300 个 open Issue，以及最近更新的 100 个 closed Issue。列表页还读最近 30 天的 Issue 评论（最多 3 页）计算首次响应时间；这三组读取对每个仓库并发进行。侧栏的待分诊角标只读 open Issue，不读评论和 closed Issue，在外壳给 GitHub 的 5 秒预算内完成。
- **用户反馈**：App 内「反馈」提交的内容（`POST /v1/feedback`，见 [用户体系](user-auth.md#app-内反馈)），按状态（`new` 待处理、`resolved` 已处理）、类型（`bug`、`suggestion`、`dictionary`）和平台（`android`、`ios`、`macos`、`windows`、`linux`、`harmony`）筛选，其他筛选值返回 400 `invalid_filter`。每行带提交者昵称、是否匿名账号（只有设备匿名身份的账号）、截图张数和用户勾选附带的诊断信息（只含服务端白名单里的键）。截图经 `GET /api/feedback/{id}/screenshots/{0–2}` 读取，响应 `Cache-Control: no-store`、`Content-Security-Policy: sandbox; default-src 'none'`，页面转成 `data:` URL 显示；截图不进头像存储，也没有公开地址。操作 `resolve_feedback`、`reopen_feedback`（`{"id"}`，需要 `triage_issues`，写审计，互为撤销）。反馈保留 180 天，由 `Store.Prune` 连同截图一起删除，注销账号时级联删除。
- **敏感词库**：规则是普通词或 RE2 正则（不超过 200 个字符，不能匹配空文本；`{n}`、`{n,m}` 这类计数重复展开后合计不超过 100 步，例如 `[\pL\pN]{101}` 会被拒绝，以免一条规则拖慢所有上传的检查），分类为广告导流、低俗、辱骂、违法、自定义，处理方式为「拦截」或「需复核」，并显示近 7 天命中次数。只在大小写、全半角或空白上不同的普通词视为重复（409 `exists`）。匹配器缓存在内存中，修改最多 30 秒后在所有副本生效；词库 PR 审核这类只读预览不计入命中次数；命中计数在内存中累积，每 30 秒批量写回一次，进程正常退出时写回剩余计数；进程被强制终止时可能丢失最近 30 秒的计数。规则作用于词库投稿和社区上传，见下文。
- **用户账号**：搜索、按角色筛选（已验证邮箱是所有者或管理员成员时显示对应角色，所有者显示为维护者）、统计卡（总数、本周新增、开启设置同步的比例、已封禁数），详情抽屉显示脱敏的联系方式、登录设备（从 User-Agent 解析的平台，不显示地理位置）、作品和会话。`ban_user` 需要原因，在同一事务里封禁、吊销全部会话，并把该用户的社区内容以 `owner_banned` 下架；被封禁的账号登录、刷新令牌和会话鉴权都返回 403 `account_banned`，直接在数据库里写入的封禁也一样。`unban_user` 只恢复因 `owner_banned` 下架的内容。
- **下载记录**：按平台、版本、安装包、渠道分组，显示今日和近 7 天下载量以及国内镜像占比，均按 UTC 自然日计。客户端和官网镜像的数据来自遥测下载事件；GitHub Release 渠道取每日资产下载量快照的差值，不依赖客户端上报，但每个资产的第一次快照计为 0，快照之前的下载不计入。页面底部的「官网下载镜像」维护官网下载页上 Windows 安装包的蓝奏云盘链接（旧路径 `/site-settings` 重定向到这里）：`GET /api/site-settings` 返回 `{"lanzou_url","updated_at","updated_by"}`，`POST /api/site-settings {"lanzou_url"}` 保存，空字符串表示清空。链接必须是带主机名的 `https://` 绝对地址，不含账号密码，最长 512 字节，否则返回 400 `invalid_lanzou_url`。修改与审计（`set_lanzou_url` / `clear_lanzou_url`）同事务写入，清空后仍保留最近修改时间和操作者。
- **公告推送**：标题（不超过 200 字）、正文、投放平台（全部，或 windows、macos、linux、android、ios、harmony）和渠道（官网横幅 `site`、App 内通知 `app`、Telegram）。正文最多 20000 字，页面在发送前检查。草稿可以反复编辑，切换到另一条草稿前会提示放弃未保存的修改。发布时勾选了 Telegram 的，先调用 Bot API `sendMessage`，失败则整条公告不发布。归档没有撤销。「触达人数」需要客户端回执，显示「—」。
- **发布管理**：每个平台一张卡片，显示最新版本、状态和检查清单。「CI 全部通过」取自 tag 所在提交的 check run；「签名与公证」只有 workflow 里存在名为 `sign` 的 check run 时才显示；「更新日志已填写」看 release 说明是否为空；需要商店 API 的平台，商店一项显示「需手动」。历史版本从每个仓库最近 100 个 release 中按 `tag_prefix` 过滤：草稿为「待发布」，prerelease 为「公开测试」，正式版为「已发布」，带撤回标记的为「已撤回」。说明按 `### 新增 / 修复 / 改进 / 说明 / 待办` 分类显示。可以触发发布流水线、编辑说明和撤回版本；撤回会改为 prerelease、在说明开头加撤回标记，并把上一个正式版设为 latest。
- **云端监控**：每个上游服务近 24 小时（按整点滚动，不是 UTC 自然日）的调用数、P95、错误率和逐小时曲线，以及本月（UTC）用量与 `admin.services` 中额度的对比；金额按用量乘以 `unit_price` 估算。只记录服务、耗时和状态类别，不记录请求内容。
- **崩溃上报**：按签名分组（平台、规范化后的错误信息和第一个非系统栈帧，取 SHA-256 的前 16 位）。同一问题出现在不同平台时分成各自的分组，分别在对应平台的仓库跟进；平台先归一（win 归 windows，mac、darwin 归 macos，ipados 归 ios，harmony、ohos 归 harmonyos，其余转小写），所以同一平台的不同写法仍在一组，分组的平台创建后不再变化。列表显示 7 天次数、与前 7 天的环比、新出现标记和影响设备数（按 `install_id` 去重）。状态为未处理、已知问题、已修复；可以在平台对应的仓库建 Issue，状态随之改为已知问题并记录链接。Issue 已在 GitHub 创建但链接写不回数据库时，服务返回 503 `issue_not_recorded` 并带上 `issue_url`，页面在提示条中显示这个链接，并在本次打开页面期间把该分组当作已有 Issue，不再提供「建 Issue」，以免重复创建；刷新后链接只能从服务日志中找回，应先把分组标记为已知问题。已修复的分组再次崩溃时不会自动重新打开。
- **系统状态**：每 60 秒探测数据库，并汇总最近 5 个完整分钟的上游指标，错误率或 P95 超过 `slow_ms` 判为降级；每日可用分钟数保留 60 天。连续 3 次判为降级或不可用时自动开启故障事件，之后连续 5 次正常时自动关闭；同一段异常里自动事件被管理员手动关闭后不会再自动开启。也可以通过 `open_incident`、`update_incident`、`resolve_incident` 手动管理。只展示数据库和已配置的上游服务。多副本部署时的分工见下方「系统状态的多副本行为」。
- **服务日志**：后端各副本的实时日志，从集群的 Loki 读取（见「配置」中的 `admin.logs`）。顶部按钮按副本筛选（全部或某一个副本，每个副本一种颜色），级别筛选为全部、INFO 及以上、WARN 及以上和 ERROR，搜索框按包含的文字筛选（区分大小写，在 Loki 侧以 `|=` 过滤）。打开时先回填最近 15 分钟内最新的 500 行，之后每 2 秒追加新行；停在底部时自动滚动，向上滚动后停止自动滚动并显示「回到最新」。暂停时连接保持，新行先缓冲，继续时一并显示。页面最多保留最近 2000 行。每行显示时间（CRI 前缀里的时间）、副本、级别和去掉 CRI 前缀与 slog 时间级别后的内容。级别取自 slog 默认格式（`2026/10/01 14:47:01 INFO …`）；筛选 WARN 或 ERROR 时排除的是低于该级别的行，所以无法识别级别的行（panic、标准库 `log` 的输出）在这两个筛选下也会显示。读取日志不写审计（后台的读操作都不写审计）。

  `GET /api/logs?since=&limit=&pod=&level=&q=` 返回窗口内最新的 `limit` 行（按时间升序，最新的在最后）：`{"lines":[{"ts","time","pod","stream","level","message"}],"pods":[…],"truncated","cursor","since","until"}`。`since` 为 Go 时长写法，1 分钟到 24 小时，默认 `15m`；`limit` 为 1–2000，默认 500；`pod` 为副本全名；`level` 为 `INFO`、`WARN` 或 `ERROR`（不区分大小写），表示该级别及以上；`q` 最多 200 字节，不能含换行等控制字符。`ts` 是 Loki 的纳秒时间戳（字符串），`cursor` 是读到的位置（窗口终点，纳秒），可以交给日志流的 `cursor` 从这里继续；`pods` 是窗口内有日志的全部副本，不受其他筛选影响（用 `count_over_time` 即时查询统计，不用标签值接口，后者会带上一两个小时前已下线的副本）。行内容超过 4 KiB 的部分截断。

  `GET /api/logs/stream` 接受同样的 `pod`、`level`、`q`、`since`，另有 `backfill`（0–1000，默认 200）和 `cursor`，返回 Server-Sent Events（`text/event-stream`）。不带 `cursor`（也没有 `Last-Event-ID` 请求头）时先回填 `since` 窗口内最新的 `backfill` 行；带上时从它之后继续，最早回到一小时前。事件：`lines`（`{"lines":[…]}`，事件 `id` 是续传游标，即服务端已经读到的位置）、`pods`（`since` 窗口内有日志的副本，连接时和之后每 30 秒一次）、`gap`（`{"from","to"}`，日志产生得比速率上限快，用满读取页数后仍落后超过 60 秒时跳到最新位置）、`error`（`{"code":"logs_unavailable"}`，Loki 暂时不可用，连续失败 5 次后关闭连接）和 `end`（`{"reason":"max_duration"}`，连接满 30 分钟后关闭，客户端带游标重连）；没有新行时每 15 秒发一行注释作为心跳。服务端每 2 秒查询一次 Loki，每次回看 5 秒并按（时间戳、副本、内容）去重，所以不同节点上 promtail 晚到 5 秒以内的行不会漏也不会重复；单个连接每次最多读 2 页、每页 500 行，即每 2 秒最多 1000 行。每个管理员最多同时打开 2 个日志流，每个副本合计最多 10 个，超出返回 429 `too_many_streams` 并带 `Retry-After: 30`。日志流不受后台 15 秒请求超时限制，打开时照常经过鉴权、来源检查和每分钟 300 次的限流（只计一次）。浏览器的 `EventSource` 不能带 `Authorization` 请求头，前端用 `fetch` 读取事件流，所以管理员密钥登录同样可用。

  两个接口的错误：功能未配置时 404 `logs_disabled`，没有 `view_logs` 时 403 `permission_denied`，参数无效时 400 `invalid_since`、`invalid_limit`、`invalid_pod`、`invalid_level`、`invalid_query`、`invalid_backfill` 或 `invalid_cursor`，Loki 不可达或返回错误时 502 `logs_unavailable`（Loki 的响应正文不透传）。用户输入只以转义后的字符串字面量进入 LogQL（`strconv.Quote`，与 Loki 解析字符串用的 `strconv.Unquote` 互逆），不能改变查询结构。
- **权限日志**：角色与权限矩阵、成员列表（所有者排在最前，显示会话数和最近活动），以及操作日志（`/api/audit`，可按 `action` 精确筛选、按 `actor` 模糊筛选，文案由 `action` 和 `detail` 生成）。
- **个人中心**：资料、本月处理量（词库 PR、社区审核、Issue）、社区审核的平均处理时长、通知偏好、最近 6 条本人操作、登录会话（可吊销其他会话）和个人访问令牌。「每周摘要」邮件尚未接入，开关置灰。

旧接口仍保留：`/api/overview?days=7|30`、`/api/users`、`/api/downloads`、`/api/crashes` 及 `resolve_crash` / `reopen_crash`、各社区列表与详情、`/api/audit`、`/api/admins`（仅所有者），以及只读的 `/api/system`（版本和各能力是否已配置，不返回上游 URL、模型名或密钥）。所有列表每页 50 条，支持 `q` 和 `page`，返回 `total` 和 `has_more`；`q` 只匹配字段的值（不匹配字段名），皮肤的设计 JSON 不参与搜索。

## 配置

### `admin.environment`

外壳顶部的环境标签，默认「生产环境」，最多 32 个字符，不能有首尾空白或换行。预发布或测试部署可以设为「测试环境」等，避免在错误的环境里操作。

### `admin.github`

后台以一个 GitHub App 的身份读写词库 PR、Issue、Release 和流水线。`app_id` 为 0 时整块只作文档用途，所有依赖 GitHub 的接口返回 404 `github_disabled`，对应页面显示「未配置」。

| 字段 | 说明 |
| --- | --- |
| `app_id`、`installation_id` | GitHub App 及其安装的 ID |
| `private_key_env` | 存放 App 私钥（PEM，PKCS#1 或 PKCS#8）的环境变量名 |
| `api_url` | 可选，默认 `https://api.github.com`，必须是 HTTPS |
| `dictionary_repo` | 词库审核的仓库。留空时跟随 `word_submissions.github.repository`，官网投稿未启用时默认 `metasequoiaime/msime-dictionary`。两者都启用时必须是同一个仓库（不区分大小写），否则服务拒绝启动：`word_submissions` 表只按 PR 编号记录投稿说明，换了仓库就会把说明挂到无关的 PR 上 |
| `issue_repos` | 问题分诊的仓库，最多 20 个 |
| `platforms[]` | 发布平台，最多 16 个，按显示顺序排列 |

`platforms[]` 的字段：

- `id`：URL 中的稳定键，小写字母、数字和连字符，最多 32 位。
- `name`：显示名。
- `repo`：`owner/name`。
- `tag_prefix`：在仓库中选出本平台 release 的 tag 前缀，例如 `windows-v`。
- `release_workflow`：触发发布的 workflow 文件名，例如 `release.yml`；留空则不能在后台触发发布。
- `assignee`：分诊时指派的 GitHub 用户，可以留空。
- `label`：本平台 Issue 的标签。

GitHub App 需要安装到上面提到的每个仓库，并授予以下仓库权限：

| 权限 | 级别 | 用途 |
| --- | --- | --- |
| Contents | Read and write | 读写词库 PR 分支的文件；列出草稿 release，修改 release 说明和状态 |
| Pull requests | Read and write | 读取、改标题、合并、评论并关闭词库 PR |
| Issues | Read and write | Issue 分诊、指派、评论、开关；为崩溃分组建 Issue |
| Actions | Read and write | 以 `workflow_dispatch` 触发发布流水线 |
| Checks | Read-only | 发布检查清单中的 CI 和 `sign` 结果 |
| Metadata | Read-only | GitHub 对所有 App 的强制要求 |

每个平台的 `release_workflow` 必须声明 `workflow_dispatch` 触发器，并有一个名为 `version` 的输入。后台以仓库默认分支为 `ref`，发送 `{"inputs":{"version":"v0.5.5"}}`：

```yaml
on:
  workflow_dispatch:
    inputs:
      version:
        description: 要发布的版本号，例如 v0.5.5
        required: true
```

缺少触发器或输入时 GitHub 返回 422，后台报 409 `workflow_rejected`；文件不存在时报 409 `workflow_not_found`；未配置 `release_workflow` 时报 409 `no_workflow`。版本号必须匹配 `^v?[0-9][0-9A-Za-z.+-]{0,62}$`，否则返回 400 `invalid_version`。

GitHub 读请求在内存中缓存 60 秒，并使用 ETag 条件请求，以免耗尽每小时 5000 次的配额。几个平台共用一个仓库时，GitHub 的 latest 是整个仓库的：撤回某个平台的最新版后，该平台的上一个正式版会成为整个仓库的 latest，可能盖过另一个平台更新的版本。所以下载站应按 `tag_prefix` 自行挑选每个平台的版本并跳过 prerelease，不要直接读 `/releases/latest`。

`admin.github` 与官网词库投稿使用的 `word_submissions.github` 是两个独立配置，可以用同一个 App，也可以分开，但两者指向的词库仓库必须相同（见上表 `dictionary_repo`）。

### `admin.services`

云端监控和系统状态页展示的上游服务，最多 32 个。不配置时，两个页面按配置文件中实际启用的上游服务和默认名称展示，但没有额度。

| 字段 | 说明 |
| --- | --- |
| `key` | 与指标记录一致的服务键：`cloud`、`chat`、`translation`、`transcription`、`streaming`、`images`、`niutrans_document`、`niutrans_image`、`niutrans_voice` |
| `name`、`provider` | 显示名（1–32 字）和服务商（不超过 64 字） |
| `quota.limit` | 月度额度，0 表示不限 |
| `quota.unit` | `calls`、`chars`、`hours` 或 `cny`。`chars` 只能用于按字符计量的 `translation`，`hours` 只能用于按秒计量的 `transcription` 和 `streaming`，`cny` 需要 `unit_price` 大于 0；不匹配时启动报错，以免额度永远显示 0% |
| `quota.period` | 只支持 `month`（UTC 自然月） |
| `quota.unit_price` | 每个计量单位的估算人民币价格，0 表示不估算费用 |
| `slow_ms` | P95 超过此值判为降级，默认 3000，范围 1–120000 |

### `admin.logs`

服务日志页读取的 Loki，按需配置：

| 字段 | 说明 |
| --- | --- |
| `loki_url` | Loki 的 HTTP 地址，例如集群内的 `http://loki.loki.svc.cluster.local:3100`。可以是 `http` 或 `https`，可以带路径前缀，不能带账号密码、查询串或片段。为空（或不写整个 `logs` 块）时功能关闭：侧栏不显示服务日志页，两个接口返回 404 `logs_disabled` |
| `selector` | LogQL 流选择器，选出后端各副本的日志流，默认 `{namespace="app",container="msime-backend"}`。只能是 `{标签="值",…}` 形式的选择器（支持 `=`、`!=`、`=~`、`!~`），最多 512 字节，不能带管道。选出的流必须有 `pod` 标签（promtail 的 `kubernetes-pods` 任务默认带）。`loki_url` 为空时不校验，可以先写好 |

服务端请求 Loki 时不带租户头（`X-Scope-OrgID`），适用于 `auth_enabled: false` 的单租户 Loki；每次请求超时 10 秒，响应体最多读 16 MiB，不跟随重定向。

上线顺序：旧版本不认识 `admin.logs`，而配置中的未知字段会让服务拒绝启动。先让所有副本都运行包含此功能的版本，再在配置里加入 `admin.logs` 并滚动重启；回退到旧版本之前，先从配置中删除 `admin.logs`。Kubernetes 部署还要确认后端所在命名空间能访问 Loki 的 3100 端口（Loki 所在命名空间的 NetworkPolicy 需要放行来自后端副本的入站流量）。

### `admin.telegram`

公告的 Telegram 渠道。`chat_id` 为空时渠道关闭，此时在后台勾选 Telegram 发布会返回 409 `telegram_disabled`。`chat_id` 是数字会话 ID 或 `@频道名`；`bot_token_env` 默认 `MSIME_ADMIN_TELEGRAM_TOKEN`，启动时校验令牌格式。Bot 需要能在目标频道或群组里发消息（频道需设为管理员）。

### 客户端跨域

`/v1/notices` 和 `/v1/telemetry/events` 受 `/v1` 中间件的来源规则约束：浏览器从其他域名请求时，该域名必须在顶层 `allowed_origins` 中，否则返回 403。官网要显示公告横幅或上报镜像下载，需要先把官网域名加进去（生产为 `https://msime.app`）。

### 客户端地址

按地址计的限额（账号和社区接口每分钟 120 次、公告和下载镜像每分钟 1200 次、遥测每分钟 60 次和每天 20 次崩溃、匿名开户每天 `auth.anonymous.daily_per_address` 次、后台登录、官网词条投稿）都用同一个客户端地址：顶层 `client_ip_header` 为空时只信任 TCP 对端；部署在反向代理后，把它设为由代理覆盖写入的头，例如 Cloudflare 的 `CF-Connecting-IP`（`X-Forwarded-For` 取最后一段）。IPv6 地址按 /64 归为一个。生产经 Cloudflare Tunnel 和 traefik 转发，TCP 对端永远是 traefik，必须设为 `CF-Connecting-IP`，否则所有用户共用一份额度。只填代理会覆盖的头，客户端自己能带的头可以伪造。旧的 `word_submissions.client_ip_header` 仍然有效，与顶层配置同时设置且不同时拒绝启动。

## 社区事后审核

社区内容采用事后审核（post-moderation）：

- 新上传的皮肤、候选皮肤、插件、词库和回复模板立即公开，状态为「待复核」（`pending`），由审核员随后复核。只有「已下架」（`removed`）的内容对作者以外的所有人隐藏：公开列表、详情、下载和评分接口都会排除它。已有内容在迁移时一律设为「已通过」。
- 上传时用敏感词库检查名称、描述等文本：命中「拦截」级规则返回 422 `blocked_content`，不保存；命中「需复核」级规则照常发布，并在待复核卡片上标出命中的词。与已发布内容完全相同的重试在检查之前直接返回成功，所以词库后来新增的规则不会让已上线内容的重试失败，也不会重复计入命中次数。
- 下架需要原因，可以撤销，撤销会恢复下架前的状态。撤销对已下架内容的通过或恢复时，后台用 `remove_content` 的 `value: {"previous":"pending"|"approved"}` 一并带回它原来的恢复状态，之后再恢复仍回到原来的状态（例如被驳回的待复核内容回到待复核）。
- 后台的「通过并上架」带上审核员看到的状态和版本：`approve_content` 的 `value: {"from":"pending"|"removed","created_at":"…","updated_at":"…"}`，`created_at` 区分作者删除后用同一 id 重新发布的内容，`updated_at` 只用于作者可以修改的候选皮肤、词库和回复模板。内容已被其他审核员处理，或作者在此期间改过、删除重发过内容，接口返回 409 `conflict` 且不做改动，审核员需要查看最新内容后再操作。不带 `value` 的调用不做这项检查。
- 封禁作者会以 `owner_banned` 下架其全部内容，只有解封能恢复。
- 举报：登录用户通过 `POST /v1/community/reports` 举报，后台卡片显示被举报次数，详情列出举报原因，每条新举报生成一条通知。
- 作者可以看到自己作品的状态：社区列表和详情带 `fields=moderation` 时，自己的作品多一个 `moderation` 字段（`approved`、`pending`、`removed`），客户端只对 `removed` 显示「已下架」徽标。皮肤和插件也支持 `scope=mine` 列出自己的作品，已下架的也在其中。他人看不到这个字段，不带参数的响应保持原样。
- 目前没有事先审核（pre-moderation）模式，也没有向作者发送下架原因的机制；确认框里填写的原因只记录在审核记录和操作日志中，接口不会返回给作者。
- 上传命中拦截级规则返回 422 `blocked_content`；敏感词库暂时无法加载时，上传返回 503 `screening_unavailable` 并带 `Retry-After`，不保存，也不会误报成账号服务不可用。

## 公开客户端接口

以下接口在 API 域名上，不在后台域名上。

### `GET /v1/notices?platform=&channel=`

不需要认证，返回最近 20 条已发布的公告：`{"items":[{"id","title","body","targets":[…],"channels":[…],"published_at"}]}`。

- `platform` 为 `windows`、`macos`、`linux`、`android`、`ios`、`harmony` 之一，匹配投放到该平台或全部平台的公告。也接受别名（不区分大小写）：`win`、`mac`、`darwin`、`ipados`、`harmonyos`、`ohos`，按对应的平台匹配。
- 正文 `body` 是简单 Markdown（后台编辑框也这样提示）。客户端渲染时必须禁用原始 HTML，链接在外部浏览器打开。
- `channel` 为 `site`、`app` 或 `telegram`。
- 参数非法返回 400 `invalid_platform` 或 `invalid_channel`。
- 响应带 `Cache-Control: public, max-age=60` 和 `Vary: Origin`，发布和归档最多 60 秒后可见。该接口与 `GET /v1/site/download-mirrors` 共用每个 IP 每分钟 1200 次的限额，与登录、刷新令牌和社区接口的每 IP 每分钟 120 次限额分开计数，所以轮询公告不会挤占登录额度；客户端不要比 max-age 更频繁地请求：在设置窗口或 App 首页打开时拉取，不要由输入法进程在后台轮询。公告以可关闭的横幅或卡片显示，关闭记录按公告 `id` 保存在本机。

### `GET /v1/site/download-mirrors`

不需要认证，供官网下载页读取：`{"lanzou_url","updated_at"}`，未设置或已清空时两个字段都是空字符串。与 `GET /v1/notices` 共用每个 IP 每分钟 1200 次的限额（与登录的 120 次限额分开计数），响应带 `Cache-Control: public, max-age=60` 和 `Vary: Origin`；官网侧再缓存约 10 分钟，所以修改后最多约 10 分钟生效。

### `POST /v1/community/reports`

需要已登录的用户会话。请求体：

```json
{"kind": "skins", "item_id": "…", "reason": "商标侵权", "detail": "可选补充说明"}
```

- `kind` 为 `skins`、`candidate-skins`、`plugins`、`dictionaries`、`replies`、`phrases` 之一（`phrases` 是社区短语包，即资源 kind `phrase`）；`reason` 1–64 字；`detail` 最多 1000 字。客户端统一使用固定的原因列表：侵权/抄袭、色情低俗、违法违规、垃圾广告、恶意插件、其他。设备的匿名账号会话也可以举报。
- 只能举报自己能看到的内容（未下架；候选皮肤必须是公开的），否则返回 404 `item_not_found`。
- 同一用户重复举报同一内容只记一次：首次返回 201，重复返回 200，响应体都是 `{"reported":true}`。
- 每个账号每小时最多举报 30 次。

### `POST /v1/telemetry/events`

不需要认证：不需要设备令牌或用户会话，带了 `Authorization` 也会被忽略，不做校验，所以未登录的客户端、msime-windows 和官网都能上报，仍带令牌的旧客户端照常工作。请求体最多 32 KiB，`Content-Type: application/json`，成功返回 `202 {"accepted":true}`。按客户端地址（见「客户端地址」）限流：每分钟 60 次，与登录和社区接口的 120 次分开计数；`crash` 每个地址每天另限 20 次，超出返回 429 和 `Retry-After: 3600`；已记录过的 ID 重试时直接返回 202，不占这 20 次。

| 字段 | 规则 |
| --- | --- |
| `id` | 必填，16–128 字符的全局唯一事件 ID，用 UUID v4；重试必须复用，同一 ID 只记录第一次 |
| `kind` | 必填，见下表 |
| `platform` | 必填，1–32 字符，用规范平台 ID：`windows`、`macos`、`linux`、`android`、`ios`、`harmony` |
| `version` | 必填，1–64 字符，真实的应用版本号 |
| `message` | 只有 `crash` 使用，且必填，最多 1000 个 Unicode 标量；异常或信号摘要，首行应有意义 |
| `stack` | 只有 `crash` 使用；帧写作模块+偏移或符号，模块路径只保留文件名，不得带出用户目录名。超过 16000 个标量时服务端在最后一个完整行处截断后照常接收 |
| `artifact` | 可选，安装包文件名，1–64 字符，单行 |
| `channel` | 可选，分发渠道，匹配 `^[a-z0-9][a-z0-9_-]{0,31}$`；后台为 `cn-mirror`、`website`、`github`、`app-store`、`testflight`、`appgallery`、`google-play` 显示中文名 |
| `install_id` | `active` 必填，其他事件也都应带上（崩溃页的影响设备数按它去重）；安装时随机生成并保存在本机的匿名 ID，16–64 位 `[A-Za-z0-9_-]`，不得来自硬件、账号或用户数据 |

只接受上表字段，未知字段返回 400 `invalid_json`。`crash` 的 `message` 和 `stack` 中的 CRLF 与单独的 CR 统一转为 LF，所以 Windows 的报告不会因控制字符被拒绝，并与 LF 的同一崩溃归入同一分组。请求体的 32 KiB 是字节数，客户端应把 stack 的 UTF-8 字节数控制在约 12 KB 以内。

| `kind` | 含义 | 用于 |
| --- | --- | --- |
| `active` | 该安装当天活跃，每个安装每个 UTC 日最多一次，`id` 用 `active-<install_id>-<yyyymmdd>`，重复发送不额外计数 | 活跃设备、各平台活跃 |
| `session` | 一次正常结束的会话：输入法宿主进程的一次生命周期，iOS 键盘为一次显示 | 无崩溃会话率 |
| `session_crash` | 一次以崩溃结束的会话。只有本地有该会话的崩溃记录（崩溃处理程序写下的）时才算；进程被系统回收、注销、关机或低内存杀掉而留下的会话标记不算崩溃 | 无崩溃会话率 = session ÷ (session + session_crash)，按近 7 天计算 |
| `crash` | 一次崩溃，带错误信息和堆栈。崩溃处理程序只写盘，下次启动再发送 | 崩溃分组：入库时计算签名并更新分组 |
| `download` | 一次安装包下载，只由官网在用户点击国内镜像（蓝奏云）链接时上报：`channel=cn-mirror`、`artifact` 为安装包文件名、`version` 为发布版本 | 下载记录、累计下载 |

客户端在下次启动时补发上一次会话的 `session` 或 `session_crash`。客户端不发 `download`：GitHub Release 的下载量取自服务端的 Release 快照，应用安装数看 `active`。如果再为 GitHub Release 下载上报 `channel=github` 的事件，这次下载会被遥测和快照各计一次。

非 `crash` 事件不能带非空的 `message` 或 `stack`。后台统计时会归一平台名（`win` → windows，`mac`、`darwin` → macos，`ipados` → ios，`harmony`、`harmonyos`、`ohos` → HarmonyOS），下载记录也按归一后的平台分组。服务端以接收时间入库，离线上报算在接收日；后台不额外存 IP 或用户身份（限流只存地址的 SHA-256）。数据保存在 PostgreSQL，多副本共享；`download` 和 `crash` 事件不自动清理，需要按规模设置归档或保留策略。

响应处理：202 已接收（重复 ID 也是 202）；400 表示事件本身不合法，丢弃，不要重试；429（遵守 `Retry-After`）、5xx 和网络错误保留事件，稍后用同一 ID 重试。离线队列最多保留 64 条。

同意：使用情况上报默认开启，用户可以在设置里关闭（各端共用偏好键 `usage_reporting`，布尔值，默认 true；旧的 `telemetry_enabled` 不再读取）。关闭后什么都不发，并清空本地队列。隐私说明和应用内文案必须如实描述发送的内容。不要上传输入内容、密码、令牌或个人信息。

浏览器（官网）上报时用 `fetch(url, {method: "POST", keepalive: true, headers: {"Content-Type": "application/json"}, body})`，不能用 `navigator.sendBeacon`：它发不出 `application/json` 的跨域请求，接口会返回 415。

`active`、`session`、`session_crash` 事件保留 90 天，由每小时的清理任务删除（概览最多读近 60 天）；`download` 和 `crash` 事件不清理，累计下载和崩溃分组依赖它们。

### 词库投稿的新错误码

官网的词库投稿接口现在可能返回：400 `blocked_word`（备注命中拦截级敏感词）、`invalid_entries.rejected` 中的 `blocked_word` 条目（词条命中拦截级敏感词），以及 503 `screening_unavailable`（敏感词检查暂时不可用）。官网表单应显示这些代码，至少回退显示 `message` 字段。

## 需要仓库外配合的改动

下列设计元素依赖客户端、官网或外部系统。后台已做好接收和展示，在对方完成之前显示空态或说明，不造假数据。

1. 活跃设备、各平台活跃、无崩溃会话率、影响设备数：各客户端需要匿名上报 `active`、`session`、`session_crash`、`crash` 事件和 `install_id`。生产配置需要设置 `client_ip_header`，否则遥测和其他按地址的限额由所有用户共用。
2. 下载记录的安装包和渠道：官网在国内镜像链接被点击时上报带 `artifact` 和 `channel=cn-mirror` 的下载事件。GitHub Release 渠道靠快照获取，客户端不上报下载。
3. 社区举报：客户端需要在展示他人作品的画廊里增加「举报」入口，调用 `POST /v1/community/reports`。
4. 公告的 App 内通知和官网横幅：App 和官网需要拉取 `GET /v1/notices`，官网域名还要加入 `allowed_origins`。在此之前公告只写入数据库，用户看不到。
5. 「下载后完成安装」比例无法测量，已去掉。
6. 公告只有 Telegram 一个推送渠道（QQ 群没有官方 Bot API）；触达人数需要客户端回执，显示「—」。
7. 登录方式是 Google OIDC，两步验证由 Google 账号控制，后台不提供 2FA、通行密钥或 GitHub 登录。
8. 登录设备不显示地理位置，这需要 GeoIP 库。
9. 没有站内信系统，用户详情不提供「发送站内信」；也没有 AI 额度模型。
10. 问题分诊不显示「官网匿名提交」和诊断附件，因为仓库里没有官网反馈端点；来源一律是 GitHub。
11. 「商店审核中」以及 iOS、HarmonyOS 的商店状态需要 App Store Connect 或 AppGallery API，检查清单显示「需手动」。
12. 发布检查清单的「签名与公证」只有 workflow 定义了名为 `sign` 的 check run 时才显示。
13. 词库仓库实际是 `metasequoiaime/msime-dictionary`。

## 审计

读取（包括服务日志）不写审计。所有写操作都写入 `admin_audit`，包括 `actor`、`action`、`target` 和 `detail`（原因、条数、新旧值等 JSON）。数据库变更与审计在同一事务里，失败不部分生效。GitHub 和 Telegram 这类外部副作用无法与数据库放在同一事务里，做法是外部调用成功后再写审计，失败则不写；因此极少数情况下会出现外部动作已生效、而审计或数据库提交失败的情况，例如 Telegram 消息已发出但公告没有标记为已发布。标记通知已读只改本人的已读记录，不写审计。

## 本地开发与验证

```sh
pnpm --dir admin-web install --frozen-lockfile
pnpm --dir admin-web check
pnpm --dir admin-web lint
pnpm --dir admin-web build
python3 admin-web/tests/csp_smoke.py
go test ./...
go build -o /tmp/msime-server ./cmd/msime-server
```

修改 `admin-web/src/` 后执行 `pnpm --dir admin-web build`，再重新编译 Go。生成的 `dist/` 随源码提交，保证直接 `go build` 也可用；CI 会重新构建并核对产物。运行镜像不需要 Node.js。

`tests/csp_smoke.py` 用 Python 版 Playwright 和 Chromium，在模拟后端上逐页打开构建产物，检查 CSP 违规、旧路径重定向和外壳弹层，并单独构建 `tests/harness` 测试共享组件（表格、确认框、提示条、抽屉、图表）。改动依赖、共享组件或构建配置后都要运行。

开发时把 Go 的 `admin.host` 设为 `admin.localhost`、`listen` 设为 `127.0.0.1:18089`，然后运行 `pnpm --dir admin-web dev`；Vite 把 `/api` 请求代理到该 Go 服务，并设置开发用的 Host 和 Origin。也可以直接访问 `http://admin.localhost:18089` 验证实际嵌入的产物。生产必须通过 HTTPS 入口访问。

PostgreSQL 集成测试需要设置 `MSIME_TEST_DATABASE_URL`，数据库名必须含 `msime_auth_test`，只能使用一次性测试库，测试会清空测试表。多个包共用一个库时，用 `go test -p 1 ./...` 串行运行。

## 系统状态的多副本行为

- 每个副本每分钟开始后约 2 秒探测一次数据库（记为 `database` 服务的一次调用），并把本副本的调用计数写入数据库：按小时写入 `admin_service_metrics`（云端监控和当日 P95 使用），按分钟写入 `admin_service_minutes`（调用数、失败数和固定分桶的耗时直方图，各副本累加到同一行）。进程正常退出时也会写回剩余计数。
- 只有持有状态探测主锁的副本（主副本）做判定：每分钟开始后约 20 秒，读取所有副本最近 5 个完整分钟的计数，判定每个服务的状态，写入 `admin_service_verdicts`（每个服务每分钟一行），同时把这一分钟计入 `admin_service_daily`，并开启或关闭自动故障事件。主锁是 PostgreSQL 会话级 advisory lock，持有在一条从连接池取出的专用连接上，键包含当前 schema，同一数据库里不同 schema 的部署各自选主。这条连接不归还连接池，主副本因此比其他副本多占用一条数据库连接。
- 主副本退出时释放主锁；主副本所在节点失联时，PostgreSQL 依靠该连接上设置的 TCP keepalive（约 1 分钟）结束会话并释放主锁。其他副本每分钟都会尝试获取主锁，所以通常在 1–2 分钟内接手；接手期间状态可能停留在上一分钟，超过 3 分钟没有新判定时外壳显示「状态未知」。数据库连接必须直连 PostgreSQL 或经过会话级连接池，事务级连接池（如 PgBouncer transaction 模式）下会话锁无效。
- 连续异常次数、连续正常次数和「本段异常是否已开过事件」存在每分钟的判定里，新主副本从数据库接着计数，不从内存重新开始；是否有未关闭的自动事件每次都从 `admin_incidents` 读取，所以一个副本不会因为自己内存里的旧状态关闭另一个副本开启的事件。`admin_service_verdicts` 以（分钟，服务）为主键，同一分钟只记一次，即使短时间内出现两个主副本，每日可用分钟数也不会重复累加。
- `GET /api/status`、`GET /api/cloud` 和 `GET /api/shell` 的状态都读取数据库中最新一分钟的判定，任何副本回答都相同。读取失败时（该副本连不上存放状态的数据库），`GET /api/status` 和 `GET /api/cloud` 返回 503 `auth_unavailable`，`GET /api/shell` 的状态报告 `down`。
- 与单副本时的差别：判定窗口是最近 5 个完整分钟，不含当前未满的一分钟，所以异常最多晚约 1 分钟被发现；P95 由各副本的分桶直方图相加后在桶内线性插值估算，与单副本时的算法相同，精度受分桶边界限制（例如 2–3 秒之间的 P95 只能估到这个桶内）。写入晚于判定时间（每分钟第 20 秒）的计数不进入这一分钟的判定，但仍计入之后 4 次判定的窗口。
- 按分钟的计数和判定只保留 3 小时，由主副本每小时清理一次；数据库不可用时各副本在内存里保留最多 1 小时的未写入分钟计数。
- 滚动升级期间旧版本副本仍按旧逻辑在本副本内判定并直接累加每日分钟数，与新版本并存的几分钟内当天的可用分钟数可能多计，自动事件也可能被旧副本按其内存状态开关；全部副本升级后恢复正常。新增的两张表只被新版本读写，旧版本不受影响。
- 未启用 `auth`（没有数据库）时管理后台不能启用，状态探测不会运行，与之前相同。

## 已知限制

- 多副本部署：敏感词和 GitHub 缓存都在各副本的内存中，最多有 30–60 秒的不一致（词库 PR 和新 Issue 的通知按数据库去重，不会因重启或多副本重复；后台限流按数据库共享，见「限流与通用约定」）。系统状态由一个主副本汇总所有副本的指标后统一判定，见「系统状态的多副本行为」；主副本失联后最多约 2 分钟没有新的判定。
- 数据库本身不可用时，宕机分钟数和自动故障事件无法写入，`/api/status` 返回 503，外壳状态显示 `down`。
- 词库 PR 只读最近 100 个；条目比较以 PR 的 `base.sha` 为准，分支创建后主干上删除的行会显示为新增。驳回时如果评论成功而关闭失败，重试会再评论一次。
- 发布历史每个仓库只读最近 100 个 release，共用仓库的平台多时，较早的版本会从历史和每日快照中消失。并发撤回或编辑同一个 release 以最后一次为准。
- GitHub Release 的「今日」是当天快照与前一天快照的差值，快照任务在 UTC 清晨运行时主要反映前一天的下载；页面会显示「快照截至」日期。
- 社区内容下架后恢复（或因解封恢复）到待复核时，自动检查标记按当前敏感词库重新计算，可能与上传时不同。
- 弹窗打开之前出现的提示条，其「撤销」仍可用鼠标点击，但弹窗打开期间辅助技术读不到它。弹窗打开之后出现的提示条不会吞掉 Escape，按 Escape 关闭的是弹窗。

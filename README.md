# 水杉云（msime-cloud）

水杉云是水杉输入法的共通网络后端，使用 Go 实现；HTTP 服务基于标准库，WebSocket 使用固定版本的 [coder/websocket](https://github.com/coder/websocket)。集中保存服务商凭据，向 Windows、macOS、iOS 和 Linux 提供 HTTP API 和实时语音 WebSocket。平台继续负责本地输入、焦点、候选合并、麦克风权限及上屏。

当前服务与各平台源码接入已实现；已通过真实客户端到合成上游的网络测试，原生宿主验收仍在进行。完整需求与未完成项见 [需求核对](docs/requirements.md)。

## 启动

需要 Go 1.26.9 或更高版本。Docker 构建和 CI 使用同一最低版本，包含已发布的标准库安全修复；CI 通过 `govulncheck` 检查项目可达的已知 Go 漏洞。

```sh
cp config.example.json config.json
export MSIME_CLIENT_TOKEN="$(openssl rand -hex 32)"
go run ./cmd/msime-server -config config.json
```

客户端用 `Authorization: Bearer <MSIME_CLIENT_TOKEN>` 访问。每台设备分配独立随机令牌和客户端 ID；添加到 `clients` 数组，通过不同环境变量注入。不要把服务商密钥或公共共享令牌编入客户端。更换配置和令牌后重启服务。

```sh
curl -H "Authorization: Bearer $MSIME_CLIENT_TOKEN" \
  http://127.0.0.1:8080/v1/capabilities
curl -G -H "Authorization: Bearer $MSIME_CLIENT_TOKEN" \
  --data-urlencode 'text=nihao' http://127.0.0.1:8080/v1/cloud/candidates
```

`config.example.json` 仅启用云候选。配置其他功能时填入管理员控制的完整 HTTPS 接口地址、服务端模型及 `token_env` 指向的环境变量；空 URL 表示关闭功能，返回 503。对外部署需由 TLS 反向代理暴露 HTTPS，限制公网端口只到反向代理。容器内把 `listen` 改为 `0.0.0.0:8080`，只读挂载配置到 `/config/config.json` 并注入令牌环境变量。Dockerfile 已通过构建；`python3 scripts/smoke_container.py --image msime-server:verification` 可验证只读容器的启动、鉴权、功能查询和优雅关闭。

## 接口

业务接口除 `GET /healthz`、官网词条提交（见下文）、公告、社区浏览和匿名遥测上报（见 [管理后台文档](docs/admin.md#公开客户端接口)）外均需 Bearer 鉴权；Swagger 和 OpenAPI 文档可匿名访问。响应禁用缓存，输入正文、音频和凭据不写日志或磁盘。

请求日志：每个请求写一行 INFO 级 slog（`msg="http request"`），字段只有 `method`（非标准方法记为 `OTHER`）、`route`（路由模式，例如 `GET /v1/cloud/candidates`，不是原始 URL，路径参数和查询串都不记；管理后台记为 `admin:/api/<第一段>`，后面还有路径时加 `/*`，例如 `admin:/api/users/*`）、`status`、`duration_ms` 和 `bytes`（响应字节数）；5xx 记为 WARN。不记请求正文、音频、凭据、IP 或用户 ID。`/healthz` 和管理后台的日志流 `/api/logs/stream` 不记。顶层 `request_log` 默认开启，设为 `false` 关闭（云候选几乎每次按键请求一次，日志量随之增长；每个请求的额外开销是一次小对象分配和一行日志输出）。旧版本不认识 `request_log`，与 `replicas` 一样，要等所有副本都运行新版本后才能写进配置，回退前先删除。

| 方法与路径 | 请求 | 响应 |
|---|---|---|
| GET `/healthz` | 无 | `{"status":"ok"}`，进程存活检查 |
| GET `/v1/capabilities` | 无 | API 版本、已配置功能开关 |
| GET `/v1/cloud/candidates` | `text`，`scheme=pinyin\|japanese`，`limit=1..10` | `{"candidates":["你好"]}` |
| GET `/v1/models` | 无 | 可选模型列表与 `default_model`，需登录或设备令牌 |
| POST `/v1/chat/completions` | `messages`、可选 `model`、`max_tokens`、`temperature`、`stream` | Chat Completions JSON；`stream: true` 时为 OpenAI 兼容 SSE；支持联想和语音润色 |
| POST `/v1/translate` | `text`、`source_lang`、`target_lang` | DeepLX 兼容 `{"code":200,"data":"..."}` |
| POST `/v1/audio/transcriptions` | multipart `file`（WAV）、可选 `model`、`language`、`response_format=json` | `{"text":"..."}` |
| POST `/v1/niutrans/documents` | multipart `file`、`from`、`to`，可选领域/术语/记忆参数 | 小牛文档翻译任务 |
| GET `/v1/niutrans/documents/{file_no}` | 小牛文档任务编号 | 查询翻译状态 |
| PUT `/v1/niutrans/documents/{file_no}/interrupt` | 小牛文档任务编号 | 终止翻译任务 |
| DELETE `/v1/niutrans/documents/{file_no}` | 小牛文档任务编号 | 删除任务 |
| GET `/v1/niutrans/documents/{file_no}/download` | `type=0..5` | 下载文档结果 |
| POST `/v1/niutrans/images` | multipart `file`、`from`、`to` | 小牛图片翻译任务 |
| GET `/v1/niutrans/images/{file_no}` | 小牛图片任务编号 | 查询图片翻译状态 |
| GET `/v1/niutrans/images/{file_no}/download` | `type=0..5` | 下载图片结果 |
| POST `/v1/niutrans/voice` | multipart `file`、`from`、`to` | 小牛语音翻译任务 |
| GET `/v1/niutrans/voice/{file_no}` | 小牛语音任务编号 | 查询语音翻译状态 |
| GET `/v1/niutrans/voice/{file_no}/download` | `type=0..5` | 下载语音结果 |
| GET `/v1/niutrans/resources` | `action` | 调用已配置的小牛资源管理动作 |

语音模型由服务端固定。聊天默认保持固定模型；管理员可在 `chat.models` 配置最多 32 个可选模型 ID，`GET /v1/models` 返回默认模型和允许列表，聊天请求只能选择其中的模型。EveryAPI 凭据仅保存在服务端；聊天未指定输出长度时最多生成 2048 token，避免长语音润色被候选场景的小预算截断；现有 AI 候选客户端明确请求 512 token，请求上限为 2048。聊天只接受文本消息（system/user/assistant），不支持工具调用。支持现有 Linux/Windows 请求中的 `response_format.type=json_object` 或 `text`；客户端的 `thinking.type=disabled` 和 `enable_thinking=false` 只作兼容接收，不透传服务商专有字段。JSON 请求上限 64 KiB；语音文件上限 15 MiB，multipart 总体上限 16 MiB；上游响应上限 1 MiB。客户端必须保留取消和输入代次校验，失败时继续本地输入。

备用模型：客户端不再让用户选聊天模型，由服务端决定并兜底。`chat.fallback_model` 是可选的备用模型 ID（不含空白，最长 200 字节，设置时 `chat.url` 不能为空；不需要列进 `chat.models`，也不出现在 `GET /v1/models` 里），为空时不重试。本次请求的模型——省略 `model` 时的 `chat.model`、客户端显式发来的默认模型，或从 `chat.models` 中选的其他模型——上游失败时，用同样的消息、只把模型换成备用模型，再请求一次，客户端只看到第二次的结果。会重试的失败是原本返回 502 `upstream_failure` 的那些：上游连不上、返回非 2xx（包括上游的 4xx，换一个模型可能就接受了）、正文不是有效 JSON、回答没有非空白的 `choices[0].message.content`。不重试的情况：504 `upstream_timeout`（请求已到 `timeout_seconds` 截止时间或客户端已断开，没有时间再答一次）；请求在到达上游之前就被拒绝的 4xx（参数校验、`unsupported_model` 等调用方的错误，换模型不会变）；备用模型就是刚失败的那个模型。备用模型也失败时照常返回 502/504。流式请求只在第一个字节之前换模型（这之前的失败本来就回 JSON 错误，客户端什么都没收到）；开始输出后上游失败照旧发 `upstream_error` 事件后断开，不换模型，避免把两个模型的回答拼在一起。每次上游请求各计一次 `chat` 调用，主模型失败、备用模型成功会在管理后台记一次失败、一次成功；客户端的请求额度按 HTTP 请求扣，重试不会多扣。这是新增的配置字段，要等所有副本都运行支持它的版本之后再写进配置；回退前先删除。

聊天请求带 `"stream": true` 时，请求体、鉴权、校验、模型允许列表和限流与非流式完全相同，响应改为 `200`、`Content-Type: text/event-stream; charset=utf-8`、`Cache-Control: no-cache` 的 OpenAI 兼容 SSE：每个事件是 `data: <chat.completion.chunk JSON>` 加一个空行，最后是 `data: [DONE]`。服务端向上游发同样带 `stream: true` 的请求，逐个事件读取、重新编码后立即 flush；转发的 chunk 只含 `id`、`object`、`created`、`model` 和 `choices[0]` 的 `delta.role`、`delta.content`、`finish_reason`，上游的其他字段和 `choices` 为空的 usage 事件不转发。累计正文出现非空白字符之前不输出任何字节，这之前的失败（上游连不上、非 2xx、出错事件、没有正文就结束）与非流式一样返回 JSON 错误和 502/504。开始输出后上游失败（出错事件、无效事件、连接中断、超过上限）时发 `data: {"error":{"code":"upstream_error"}}` 后关闭连接，不发 `[DONE]`，日志只记原因，不记上游正文。整条流受 `timeout_seconds`、上游响应 1 MiB 和输出正文 64 KiB 的上限约束；客户端断开时上游请求随之取消。计量与非流式相同：每条流计一次 `chat` 调用，耗时取整条流的时长，客户端放弃的流不计；服务端没有按 token 计费，因此不向上游请求 `stream_options.include_usage`。客户端发 `stream_options` 等未知字段仍返回 400 `invalid_json`。

不支持流式的旧版本对 `stream: true` 返回 400 `invalid_chat_request`，客户端收到后改用非流式请求。这个功能不新增配置字段；滚动升级期间请求会轮询到新旧副本，旧副本返回的 400 由客户端的回退处理，所以无需等全部副本升级。

翻译上游支持 OpenAI 兼容聊天模型、DeepLX、DeepL、腾讯 TMT `TextTranslateBatch` 与小牛翻译 NiuTrans v2；客户端统一使用 DeepLX 格式，不持有供应商密钥。语音上游使用 multipart 转写协议，可解析 `text`、`transcription` 和 `result.text`。云候选上游使用 Google Input Tools 的响应格式，输出过滤控制字符、超长候选和重复项。

客户端请求不需要传 `provider` 字段。翻译 provider 由服务端配置决定；可选的 `translation_fallbacks` 按配置顺序尝试。主 provider 返回上游错误、超时或无效响应时，服务端继续下一个 provider；请求参数错误不会切换。所有 provider 都接受 `texts` 批量请求（最多 32 条），返回与 `texts` 等长、顺序相同的 `data` 数组：腾讯 TMT 和 DeepL 一次请求上游，其余一条一请求的 provider 由服务端拆成单条，最多 4 条同时请求上游，任何一条失败整次按失败返回（5xx 照常切换到下一个 provider），不会返回一半有译文的结果。

错误采用 `{"error":{"code":"...","message":"..."}}`：400 参数不合法、401 未认证、403 来源不允许、413 上传过大、415 格式不支持、429 限流、502 上游失败、503 功能关闭或并发已满、504 超时。错误不透传服务商正文。429/并发已满提供 Retry-After。额度按认证主体的 token bucket 控制：设备令牌按 `clients[].requests_per_minute`，登录用户每分钟 120 次。桶保存在每个副本的进程内存里，候选这类几乎逐键请求的热路径不写数据库，重启会重置。顶层 `replicas`（默认 1，可设 1–64）填部署的副本数，每个副本对每个主体执行 ⌈额度 / `replicas`⌉ 次/分钟（至少 1 次），突发容量同样按这个份额计；在无粘性会话的轮询转发下，所有副本合计约等于配置的额度。向上取整会让合计略高（例如 5 次/分钟、2 个副本时合计最多 6 次），而单个客户端的请求没有被均匀分到各副本时，可能在合计用满前就收到 429。`replicas` 必须随扩缩容同步修改并滚动重启，否则合计会按比例偏离配置。详见下文「多副本部署」。

浏览器访问须明确配置 HTTPS `allowed_origins`，不使用通配来源。原生客户端不需要 Origin。官网目前负责展示与下载，没有在线输入功能，不应添加不需要的云调用。

## 多副本部署

服务可以在同一个 PostgreSQL 后面运行多个副本（例如 k8s 多副本经 Cloudflare Tunnel 轮询转发、无粘性会话）。多副本部署必须启用用户体系和数据库（`auth.enabled`）：没有数据库时，本该经数据库共享的限流、AI 插画任务等状态只能退回各进程内存，副本之间互不可见，所以 `replicas` 大于 1 而未启用 `auth.enabled` 时服务拒绝启动。顶层 `replicas` 填实际副本数。

- 经 PostgreSQL 共享：账号、会话和社区数据；`auth_rates` 上的数据库限流（登录、社区接口、官网词条投稿、管理后台的 `admin`/`admin-auth`/`admin-login` 三项限额等，固定一分钟窗口）；共享译文缓存；遥测和管理后台数据；AI 插画任务存在 `skin_jobs` 表，任何副本都能轮询和取消，每主体与全局上限按整个部署计算（见[皮肤社区](docs/skin-community.md)）；官网词条投稿写 GitHub 前取跨副本的 advisory lock，同一时刻只有一个副本改滚动分支；系统状态由持有 advisory lock 的一个主副本汇总所有副本的分钟计数后统一判定（见[管理后台](docs/admin.md)「系统状态的多副本行为」）；数据库迁移在 advisory lock 下串行执行，多个副本同时启动也只会建一次表。
- 新增的配置字段 `admin.logs`、`request_log` 和 `chat.fallback_model` 与 `replicas` 一样：先让所有副本运行新版本，再写进配置；回退前先删除。新版本启动时会替换 `admin_role_permissions` 的权限约束（加入 `view_logs`），旧版本副本不受影响；按最小权限部署时这一步也要先用有 DDL 权限的账号执行 `-migrate-users`。
- 滚动升级时新旧版本短暂并存：旧版本副本仍在本进程内保存 AI 插画任务（新旧副本互相看不到对方的任务，客户端可能收到 404 后重新生成）、不取词条投稿锁、按旧逻辑各自判定系统状态（当天可用分钟数可能多计）。所有副本升级完成后恢复。旧版本不认识顶层 `replicas`，而配置中的未知字段会让服务拒绝启动：先让所有副本都运行新版本，再在配置里加入 `replicas` 并滚动重启；回退到旧版本（如 `kubectl rollout undo`，它不会回退单独管理的 ConfigMap）之前，先从配置中删除 `replicas`，否则重启的旧版本副本会反复启动失败。按最小权限部署时，先用有 DDL 权限的账号执行 `-migrate-users`，再给运行角色授予新表 `skin_jobs`、`admin_service_minutes`、`admin_service_verdicts` 的 `SELECT, INSERT, UPDATE, DELETE`。
- 顶层 `client_ip_header` 同理：旧版本不认识它。上线顺序是先让所有副本运行新版本，再在配置里加入 `"client_ip_header": "CF-Connecting-IP"` 并滚动重启；回退到旧版本之前先删除它。过渡期也可以继续只写旧的 `word_submissions.client_ip_header`，新旧版本都接受，新版本会把它用于所有按地址的限额。生产经 Cloudflare Tunnel 和 traefik 转发，TCP 对端始终是 traefik，不设置这一项时匿名遥测、匿名开户和账号接口的按地址限额由全体用户共用一份。
- 顶层 `site_proxy_secret_env`（官网 BFF 的共享密钥所在的环境变量名，见 [用户体系](docs/user-auth.md)）同理：旧版本不认识它。上线顺序是先让所有副本运行新版本，再把密钥写进部署的 Secret（与其他密钥一样以环境变量注入，例如 `MSIME_SITE_PROXY_SECRET`，值用 `openssl rand -hex 32` 生成，并在官网 Pages 设为同值的 Secret `SITE_PROXY_SECRET`），然后在配置里加入 `"site_proxy_secret_env": "MSIME_SITE_PROXY_SECRET"` 并滚动重启；配置了变量名但读不到 32–256 个可见 ASCII 字符时服务拒绝启动。回退到旧版本之前先删除这一项。轮换密钥时先改后端 Secret 并滚动重启、再改官网，后端不支持新旧两个密钥并存，所以在两边改完之前，官网的所有访客共用 Cloudflare 出口地址的一份按地址额度（账户接口每分钟 120 次），流量大时会出现 429；轮换宜选在低峰期，并尽快改完官网。
- 社区收藏和刷新令牌宽限期（`community_skin_saves`、`community_candidate_skin_saves`、`community_plugin_saves` 三张新表，`auth_used_refresh` 新增 `used_at` 列）：运行角色有 DDL 权限时启动会自动迁移；按最小权限部署时先用有 DDL 权限的账号执行 `-migrate-users`，再给运行角色授予三张新表的 `SELECT, INSERT, UPDATE, DELETE`。旧版本副本不读新表，插入 `auth_used_refresh` 时由 `used_at` 的默认值补上时间，滚动升级期间可以并存；落到旧副本的 30 秒内重放仍按旧规则撤销会话。
- 我的设备、常用语同步、社区短语包与 App 内反馈（`user_phrases`、`feedback`、`feedback_screenshots` 三张新表；`community_resources.kind` 与 `community_reports.kind` 的约束加入 `phrase` / `phrases`）：新版本启动时 `Ready` 发现缺表或约束是旧的，就在 advisory lock 下补建表、按 `pg_constraint` 里的约束名幂等替换两条约束；按最小权限部署时先用有 DDL 权限的账号执行 `-migrate-users`，再给运行角色授予三张新表的 `SELECT, INSERT, UPDATE, DELETE`，否则服务拒绝启动。旧版本副本不读新表、不写 `phrase`，滚动升级期间可以并存；新路由（`/v1/users/me/sessions`、`/v1/users/me/phrases`、`/v1/feedback`）落到旧副本时返回 404，客户端按功能不可用处理并稍后重试。接口说明见 [用户体系](docs/user-auth.md) 与 [社区资源](docs/community-resources.md)。
- 安卓重做批次二（Apple 网页登录、云端数据、下载链接、云剪贴板保留与置顶、AI 插画每日配额、语音贡献）：`auth_challenges` 新增 `code_challenge`、`verified_subject`、`verified_email`、`verified_email_verified`、`verified_name`、`grant_hash`、`granted_at` 七列，`user_clipboard_settings` 新增 `retention_days`，`user_clipboard` 新增 `pinned`、`device`，新表 `voice_contributions`。新列都是 `ADD COLUMN IF NOT EXISTS` 并带默认值，旧版本副本插入时不写它们，滚动升级期间可以并存；新版本启动时 `Ready` 发现缺列或缺表就在 advisory lock 下迁移。按最小权限部署时先用有 DDL 权限的账号执行 `-migrate-users`，再给运行角色授予新表 `voice_contributions` 的 `SELECT, INSERT, UPDATE, DELETE`，否则服务拒绝启动。新路由（`/v1/auth/apple/web*`、`/v1/auth/apple/callback`、`/v1/users/me/data*`、`/v1/users/me/download-link`、`/v1/users/me/clipboard/retention`、`/v1/users/me/clipboard/{id}/pin`、`/v1/voice/contributions`）落到旧副本时返回 404，客户端按功能不可用处理。AI 插画的每日配额（每个主体每天 10 个任务，`auth_rates` 键 `skin-art-day:<owner 摘要>`）是代码里的常量，不新增配置字段，所以没有配置项的上线顺序问题；旧版本副本不计这个配额，升级期间落到旧副本的提交不计数。
- 安卓重做批次三（诊断快照与远程 MCP）：新表 `diagnostic_snapshots`、`diagnostic_accesses`。新版本启动时 `Ready` 发现缺表就在 advisory lock 下建表；按最小权限部署时先用有 DDL 权限的账号执行 `-migrate-users`，再给运行角色授予这两张表的 `SELECT, INSERT, UPDATE, DELETE`，以及序列 `diagnostic_accesses_id_seq` 的 `USAGE, SELECT`，否则服务拒绝启动。旧版本副本不读新表；新路由（`/v1/users/me/diagnostics*`、`/mcp/s/{id}`）落到旧副本时返回 404，客户端按功能不可用处理。MCP 端点用无状态的 Streamable HTTP（官方 Go SDK `github.com/modelcontextprotocol/go-sdk`），不依赖粘性会话；每份快照每分钟 60 次的限额在 `auth_rates` 里按整个部署计算。不新增配置字段。详见 [诊断快照与远程 MCP](docs/diagnostics.md)。
- Apple 网页登录（安卓）的上线顺序：1）把带新路由和新列的后端部署到全部副本；2）在 Apple Developer 为 Services ID `app.msime.signin` 登记 Return URL `https://api.msime.app/v1/auth/apple/callback`，再把 `app.msime.signin` 加进生产的 `auth.apple.client_ids`（不需要 .p8 私钥）并滚动重启；3）此后 `GET /v1/auth/providers` 才返回 `apple_web: true`，只有 Services ID 在 `client_ids` 里时它才为真；4）客户端按 `apple_web` 显示 Apple 按钮。顺序反过来时，客户端看到的是 `apple_web: false`，不会把用户带到一个回调落在旧副本上的流程里。回调是全站唯一不做 Origin 检查的路由（Apple 的 form_post 带 `Origin: https://appleid.apple.com`），不要把 `appleid.apple.com` 加进 `allowed_origins`，那样会给它开 CORS。详见 [用户体系](docs/user-auth.md#apple-网页登录安卓)。
- 候选窗皮肤保留 ID（`candidate_skins_id_check`、`community_candidate_skins_package_id_check` 两条约束加入 msime-windows 的 `autumn_osmanthus`、`microsoft` 与 `default`）：新版本启动时 `Ready` 发现约束是旧的，就在 advisory lock 下替换这两条约束；按最小权限部署时先用有 DDL 权限的账号执行 `-migrate-users`，否则运行角色无权改约束，服务拒绝启动。新约束以 `NOT VALID` 添加，已有行不回头校验；若库里已有用这三个 ID 的行，它们照常保留和下载，但任何改写该行的 `UPDATE`（审核、改可见性等）都会因约束失败，需要时由管理员删除后让作者换 ID 重新发布。旧版本副本不检查这两条约束，滚动升级期间可以并存。
- 个人词库的 98 版五笔（`user_dictionary_entries_kind_check` 约束加入 `wubi98`）：安卓客户端导出的快照里 98 版五笔词条的 kind 是 `wubi98`，此前快照解码、引擎校验和这条约束都不认它，含这类词条的快照恢复每次都返回 400 `invalid_dictionary_snapshot`。新版本启动时 `Ready` 发现约束不含 `wubi98`，就在 advisory lock 下按约束名替换它；按最小权限部署时先用有 DDL 权限的账号执行 `-migrate-users`，否则运行角色无权改约束，服务拒绝启动。新约束只放宽取值，以 `NOT VALID` 添加，不扫描、不改写已有行，替换时短暂持有该表的排他锁。服务端要和能识别 `wubi98` 的引擎（`third_party/msime` 子模块）一起发布：旧引擎在校验时就拒绝 `wubi98`，所以库里不会出现这类行；但已经存了 `wubi98` 词条的库不能再回退到旧引擎，否则这些用户的个人词库查询会返回 502 `engine_failure`。旧版本副本不写 `wubi98`，滚动升级期间可以并存，落到旧副本的请求仍返回 400，客户端下一轮同步重试即可。
- 顶层 `qq_bot`（QQ 群反馈机器人，见下文「QQ 群反馈机器人」）同理：旧版本不认识它。上线顺序是先让所有副本运行新版本，再把 AppSecret 写进部署的 Secret（以环境变量 `MSIME_QQ_BOT_SECRET` 注入）、在配置里加入 `qq_bot` 并滚动重启，最后才在 QQ 开放平台填写回调地址；回退到旧版本之前先删除它。消息去重和每日限额都记在 `auth_rates`，所有副本共享，不新增表。
- QQ 群答疑的追问记录（新表 `qq_conversations`，主键是随机字符串，不用序列）：新版本启动时 `Ready` 发现缺表就在 advisory lock 下建表；按最小权限部署时先用有 DDL 权限的账号执行 `-migrate-users`，运行角色经表所有者的 default privileges 获得 DML，否则要手工授予这张表的 `SELECT, INSERT, UPDATE, DELETE`。旧版本副本不读这张表，滚动升级期间可以并存。不新增配置字段。
- 云剪贴板端到端加密（新表 `user_clipboard_devices`、`user_clipboard_wraps`；`user_clipboard` 新增 `ciphertext`、`key_id`，`user_clipboard_settings` 新增 `key_id`）：新版本启动时 `Ready` 发现缺列或缺表就在 advisory lock 下迁移，迁移同时删除全部明文条目（`ciphertext` 为空字符串的行）。按最小权限部署时先用有 DDL 权限的账号执行 `-migrate-users`，再给运行角色授予两张新表的 `SELECT, INSERT, UPDATE, DELETE`（配置了 default privileges 时自动获得），否则服务拒绝启动。新列都是 `ADD COLUMN IF NOT EXISTS` 并带默认值，`text` / `text_hash` 两列保留，滚动升级期间可以并存：旧版本副本仍接受明文上传，它写入的明文条目不出现在新版本的列表里，由新版本的写入路径和每小时的清理任务删除；旧副本的列表把密文条目显示成空文本，旧副本关闭云剪贴板时不删除密钥和设备登记（再次打开时沿用原来的密钥）。新路由（`/v1/users/me/clipboard/keys*`、`/v1/users/me/clipboard/devices/*`）落到旧副本时返回 404 或 405，密文上传落到旧副本返回 400，客户端按失败处理并稍后重试。不新增配置字段。接口说明见 [用户体系](docs/user-auth.md#云剪贴板端到端加密)。
- 按副本计算：主接口 token bucket（设备令牌和登录用户），每个副本执行 ⌈额度 / `replicas`⌉；`max_concurrent` 并发槽（实时语音会话也各占所在副本的一个）；插件发布 4 个、下载 8 个名额，原生 Engine 查询 4 路，候选皮肤图片解码 2 路，词库快照恢复 1 路；管理后台服务日志的日志流每个管理员 2 个、合计 10 个（同一个管理员的两个连接落在不同副本时可以各开 2 个）。这些上限的总量随副本数增加，设置时按单个副本的资源计算。
- 可容忍的短暂不一致：敏感词改动最多 30 秒后在所有副本生效；管理后台的全局搜索索引在各副本内存里分别建立，可能短时不同；GitHub 读缓存和同一路径的并发读取合并按副本执行，副本之间仍会各自访问 GitHub；一个副本写 GitHub 后，其他副本的 GitHub 读缓存最多 60 秒后才看到变化；上游调用指标先在内存中累积、每分钟写回数据库，副本被强制终止时最多丢失最近约 1 分钟的计数。

## 验证

```sh
go test -race ./...
go vet ./...
go build ./cmd/msime-server
```

测试使用本地 TLS 模拟服务，不需要真实服务商密钥、不消耗线上配额。测试覆盖认证、额度隔离与补充、并发上限、超时、凭据替换、模型限制、候选编码与过滤、翻译、语音 multipart、重定向与超大上游响应。

共享接口权威在本仓 `contracts/protocol.json`。它原是 MSIME-Engine `contracts/backend/protocol.json` 的副本，Engine 归档后由本仓维护。`python3 scripts/sync_contract.py --check` 检查生成常量。契约测试通过 HTTP 和 TLS 上游执行清单中每个操作的请求/响应示例。

WAV 上传现在校验 RIFF 文件长度、分块边界、fmt/data 必需块和采样帧一致性，允许 PCM 8/16/24/32 位与 IEEE 浮点 32/64 位（1–8 声道、8–192 kHz）。分块规则参考 [Microsoft RIFF 文档](https://learn.microsoft.com/en-us/windows/win32/xaudio2/resource-interchange-file-format--riff-)。缺失或空白转写文本返回 502，不再伪装成功。

本地同时检出 MSIME-Linux、MSIME-Windows（含各自 Engine 依赖）并启动 Docker 后，可运行 `python3 scripts/native_e2e.py`。脚本自动构建专用测试镜像，编译实际客户端源码，在一次性容器中访问 Go HTTPS 服务（测试 CA 只写入容器）。覆盖两端云候选、Linux AI/翻译/语音/润色、Windows 翻译与批量语音协议，以及 Apple 使用的 Engine 共享转写/润色类。此结果不代替 Windows TSF 或 Apple 签名设备验收。

### 腾讯翻译上游

将配置的 `translation` 替换为：

```json
{
  "provider": "tencent",
  "secret_id_env": "MSIME_TENCENT_SECRET_ID",
  "token_env": "MSIME_TENCENT_SECRET_KEY",
  "region": "ap-guangzhou"
}
```

密钥通过服务进程环境注入。省略 URL 时使用 `https://tmt.tencentcloudapi.com/`；如指定 URL，必须为 HTTPS 根路径且无查询参数。服务使用 TC3 签名调用现有 Windows 适配器对应的 `TextTranslateBatch`，输出仍为 `{ "code": 200, "data": "译文" }`。腾讯账户须支持该接口；当前验证使用合成上游，未调用真实账户。选择 DeepLX 时使用 `provider: "deeplx"`（或省略 provider）并填写 URL；DeepLX 的空 URL 表示禁用翻译。

### 小牛翻译上游

将配置的 `translation` 替换为：

```json
{
  "provider": "niutrans",
  "app_id_env": "MSIME_NIUTRANS_APP_ID",
  "apikey_env": "MSIME_NIUTRANS_APIKEY"
}
```

需要故障转移时，在同一配置中增加 fallback。provider 的凭据仍只从环境变量读取：

```json
{
  "provider": "niutrans",
  "app_id_env": "MSIME_NIUTRANS_APP_ID",
  "apikey_env": "MSIME_NIUTRANS_APIKEY"
}
```

```json
"translation_fallbacks": [
  {
    "provider": "tencent",
    "secret_id_env": "MSIME_TENCENT_SECRET_ID",
    "token_env": "MSIME_TENCENT_SECRET_KEY",
    "region": "ap-guangzhou"
  },
  {
    "provider": "deeplx",
    "url": "https://translator.example.invalid/translate"
  }
]
```

服务默认调用 `https://api.niutrans.com/v2/text/translate`，也可在 `url` 中指定同样的 HTTPS 接口。App ID 和 API Key 只通过服务进程环境注入；服务端按 NiuTrans v2 协议生成 `authStr`，不会把供应商凭据转发给客户端。小牛翻译接口是单条请求，批量 `texts` 由服务端拆成单条请求。语言代码会换成小牛认的写法：`zh-Hans`、`zh-CN` 换成 `zh`，`zh-Hant`、`zh-TW`、`zh-HK` 换成 `cht`，其他代码去掉地区后缀（小牛对 `zh-cn`、`en-us` 这类代码返回错误码 13007）。小牛返回错误时，日志的失败原因里带它的数字错误码，例如 `niutrans error 10001`。

小牛账号有每秒请求上限，当前账号是 5 次，超出时返回错误码 10001。`requests_per_second` 设置这个上限，小牛未设置时按 5 计算，各副本按 `replicas` 平分（向下取整，至少 1 次）。还有后备服务商时，本副本这一秒剩下的额度不够发完一个请求的全部文本，就直接交给下一家：单条请求每副本每秒最多 2 次走小牛，超过本副本额度的批量请求整批交给腾讯翻译。这样就不会再发出注定被小牛限流拒绝、只能等它失败再转后备的请求。小牛是最后一家时不受这个额度限制。升级套餐后把新的上限写进 `translation.requests_per_second`；这是新增的配置字段，要等所有副本都运行支持它的版本之后再写进配置。

文档、图片和语音接口使用同样的 NiuTrans v2 异步文件协议。服务端负责签名、上传和状态查询，客户端只看到 MSIME 设备令牌。对应 API 应用分别由 `MSIME_NIUTRANS_DOC_APP_ID`、`MSIME_NIUTRANS_IMAGE_APP_ID`、`MSIME_NIUTRANS_VOICE_APP_ID` 注入，并共用 `MSIME_NIUTRANS_APIKEY`。资源管理路由使用 `MSIME_NIUTRANS_RESOURCE_APP_ID`；`action` 只允许单段路径，服务端不会接受任意上游 URL。未配置某项 API 时，该项返回 503，不影响其他功能启动。

### 共享译文缓存

启用用户体系（PostgreSQL）时，腾讯 TMT 路径自动带上 `translation_cache` 表：命中的文本直接返回，只有未命中的才送上游，整批命中就完全不调用腾讯。候选释义一页九个词两种语言，重复率很高，这一层直接省掉对应的调用量。

这张表按内容寻址，主键是 `(source_lang, target_lang, source_text)`，**不存 `user_id`**，也不记录是谁请求的。只有不超过 64 字节、不含控制字符的短文本进缓存：契约允许 8192 字节输入是给划词翻译整段文字用的，那种请求几乎不会被第二个人原样查一遍，存下来也等于把用户打的整段内容长期留在服务端。

缓存是旁路。没启用用户体系、数据库连不上或查询超时（2 秒）时，翻译照常直连上游，不会因为缓存不可用而失败。

`hit_count` 只用来量「哪些词是真的反复查不到」。**它不是自动同步进出货词库的开关**——把机翻结果收进 `english.db` 重新分发涉及腾讯 TMT 服务条款，也意味着用户打过的字会进到所有人的词库，需要单独评估后才能做。

新增表不需要手工迁移：服务启动时发现缺表会自己补上（见下）。

### 实时语音

`GET /v1/audio/stream` 升级为 WebSocket，使用相同的 `Authorization: Bearer <device-token>` 鉴权。输入输出均为现有豆包 ASR v1 二进制消息，保留消息边界与实时结果。该入口与 multipart 批量转写分开。

管理员配置 `streaming.url` 为豆包 WSS 接口地址，例如 `wss://openspeech.bytedance.com/api/v3/sauc/bigmodel_async`，`token_env` 指向供应商 API Key 环境变量，`resource_id` 填写账户对应资源 ID。旧 App Key/Access Key 模式还须设置 `app_key_env`；设备请求无法覆盖这些上游头。空 URL 禁用功能，能力查询返回 `streaming_transcription: false`。

单条消息最大 1 MiB，每个方向每次会话累计最大 32 MiB。`max_seconds` 默认 120，可设为 1–600；会话占用所在副本的一个并发槽（`max_concurrent` 按副本计算）。任一端断开、时限到达或服务关闭时，两个连接均释放。反向代理须允许 WebSocket Upgrade，并设置至少与会话时长一致的空闲超时。不会将供应商 HTTP 错误正文或 WebSocket 关闭原因返回设备。

Windows 设置中选择“MSIME 共通后端（实时语音）”，地址填写 `wss://你的服务/v1/audio/stream`，令牌填写设备令牌；无需填写豆包 App Key。保留原有录音、实时预编辑与会话代次校验。服务端 WSS 已通过真实合作服务的合成录音验收，Windows 目标语法检查也已通过；Windows 原生录音宿主仍需独立设备验收。

在 macOS 同时检出 MSIME-Apple 后，运行 `python3 scripts/apple_e2e.py` 可编译实际 Foundation 客户端并连接 Go TLS 服务。测试证书只作为测试进程的信任锚，不修改系统信任或 Keychain；验证候选、日语、错误令牌、未受信任证书、在途取消与主线程回传。WebSocket 依赖的许可见 `THIRD_PARTY_NOTICES.txt`，容器内放在 `/licenses/`。

## 命令行（msime-cloud）

`cmd/msime-cloud` 是给 AI 助手（以及人）在终端里使用水杉云的命令行：列出与查看接口、用邮箱或短信验证码或 Google 账号登录、调用任意接口并自动附带和刷新会话。每次后端发版会把 macOS（arm64/x86_64）、Linux（x86_64/arm64）和 Windows x86_64 的构建连同 `SHA256SUMS` 附在对应的 GitHub Release 上。响应写到 stdout（JSON 自动缩进），提示写到 stderr；2xx 退出 0，服务端返回错误或调用失败退出 1（错误正文仍会打印），用法错误退出 2。

```sh
go build -o msime-cloud ./cmd/msime-cloud
./msime-cloud routes dictionaries                       # 方法、路径、鉴权类型、说明
./msime-cloud describe GET /v1/users/me/dictionaries/pinyin   # 参数、请求体与响应 schema
./msime-cloud login start --email user@example.com      # 返回 challenge_id，验证码发到邮箱
./msime-cloud login finish --challenge <id> --code 123456
./msime-cloud login google                              # 打开浏览器登录 Google，最多等待 5 分钟
./msime-cloud call GET /v1/users/me/dictionaries/pinyin -q q=你好
echo '{"display_name":"昵称"}' | ./msime-cloud call PATCH /v1/users/me -
./msime-cloud call POST /v1/community/plugins -F file=@pack.zip
./msime-cloud login admin                               # 管理员用 Google 账号登录后台，会话 8 小时
./msime-cloud call GET /api/overview -q days=7
```

`/v1` 接口的列表和说明直接取自内嵌的 `internal/server/swagger/openapi.json`，随规范自动更新；匿名接口不发送令牌。`/api` 路径发往管理后台，使用 `login admin` 保存的管理员会话，设置了 `MSIME_ADMIN_TOKEN`（管理员密钥或个人访问令牌）时改用它；登录方式和启用条件见 [管理后台文档](docs/admin.md#命令行登录)。后台路由直接取自服务端分发用的路由表，`describe` 一个 `/api` 路由时附上 `docs/admin.md` 中提到它的段落（请求体、权限）。遇到 429 且 `Retry-After` 不超过一分钟时等待后重试一次；实时语音 WebSocket 不在命令行支持范围内。默认连接 `https://api.msime.app` 与 `https://admin.msime.app`，可用 `MSIME_CLOUD_URL`、`MSIME_ADMIN_URL` 改为本地服务；设置 `MSIME_CLOUD_TOKEN` 时改用该设备令牌或访问令牌。Google 登录沿用桌面端的回环流程：命令在 `127.0.0.1` 上临时监听，只打开指向 `accounts.google.com`、回调为本机监听地址的授权页，收到 `state` 匹配的回调后再用授权码登录；`--browser false` 只打印地址，由用户自行打开。Apple 和微信登录依赖官方 SDK 或已登记的 HTTPS 回调，命令行不支持。./msime-cloud login finish --challenge <id> --code 123456
./msime-cloud login google                              # 打开浏览器登录 Google，最多等待 5 分钟
会话保存在用户配置目录下 `msime-cloud/credentials.json`（权限 0600，可用 `MSIME_CLOUD_CONFIG_DIR` 指定目录）。登录、刷新、退出和失效会话清理共用操作系统文件锁，避免并发修改丢失其他服务的会话；失效清理只删除请求实际使用的会话，不会删除并发完成的新登录。锁等待最多 15 秒，持锁进程退出后由操作系统释放，不会按文件年龄抢占仍在刷新的进程。`credentials.lock` 文件会保留，请勿在命令运行时删除它。升级命令行时先结束旧版本进程，避免两种锁协议同时操作同一凭据文件。

## Swagger / OpenAPI

文档默认关闭，生产环境保持 `docs_enabled: false`（省略时同样关闭）。只有本地开发需要调试时，在配置顶层设置 `"docs_enabled": true`，重启后访问 `/swagger/`（`/swagger` 自动跳转），规范文件为 `/openapi.json`。显式启用后的文档无需登录；关闭时页面、JS/CSS 和 OpenAPI 入口统一返回 404，即使携带有效令牌也不开放。在线 API 的鉴权不受文档开关影响。点击 **Authorize**，只填写令牌本身，再使用 **Try it out → Execute**。令牌不持久化到浏览器存储。WAV 接口提供文件上传；实时语音仅展示 WebSocket 协议，不提供 HTTP 调试按钮。页面与 Swagger UI 5.32.15 的 JS/CSS 均内嵌到二进制，无需外部 CDN，禁用外部 validator。

规范由 `scripts/generate_openapi.py` 从 `contracts/protocol.json` 和接口 schema 生成；接口更新后运行该脚本，CI 使用 `--check` 检查是否同步。Swagger UI 配置参考 https://swagger.io/docs/open-source-tools/swagger-ui/usage/configuration/ 。第三方资源版本及完整性值在 `internal/server/swagger/version.json`，许可和 NOTICE 随资源嵌入。

云候选的 `text` 是待转换的拼写。`scheme=pinyin` 时传拼音（如 `haohaoxuexi`），包含汉字返回 400 `pinyin_spelling_required`。当上游提供匹配长度时，拼音候选只保留覆盖整个输入的结果；`limit` 为最大数量，不保证凑满。启用原生引擎时，云端过滤后没有完整候选会补查 Engine 的完整词典词条（例如 `zhonguo` → `中国`）；若 Engine 确认输入需要拼写纠错且找到完整词条，也优先返回这些词条，避免上游逐字拼凑（如 `zhon'guo` → `中哦你过`）。纠错和全输入匹配由 Engine 处理，候选仍可能为空。生产验收见 [公共 API 清单](docs/windows-api-extraction.md)，早期本机测试见 [API 验证记录](docs/api-verification.md)。

### EveryAPI 合作服务：AI 联想

`chat.url` 使用 `https://api.everyapi.ai/v1/chat/completions`，`chat.model` 可配置为 `gpt-5.6-luna`，`chat.token_env` 指向保存供应商密钥的环境变量。客户端继续使用 MSIME 设备令牌。JSON object 模式会在用户输入没有明确 json 要求时追加一条格式指令，以兼容合作服务的 JSON 输出要求；原始消息内容保持不变。此接入覆盖 AI 联想和语音文本润色，不自动启用录音转写、翻译或实时语音。

### EveryAPI 合作服务：翻译与录音转写

在服务环境中设置 `MSIME_CHAT_TOKEN`，将配置的两个字段替换为：

```json
{
  "translation": {
    "provider": "openai",
    "url": "https://api.everyapi.ai/v1/chat/completions",
    "token_env": "MSIME_CHAT_TOKEN",
    "model": "gpt-5.6-luna"
  },
  "transcription": {
    "url": "https://api.everyapi.ai/v1/audio/transcriptions",
    "token_env": "MSIME_CHAT_TOKEN",
    "model": "volc.seedasr.sauc.duration"
  }
}
```

翻译由服务添加默认提示词，支持 `source_lang: "auto"`，返回格式仍为 `{"code":200,"data":"译文"}`。上游输出被截断、拒绝或为空时返回 502，不返回不完整译文。录音上传仍使用 WAV multipart；可将转写模型改为 `openai/whisper-large-v3-turbo`。这些配置仅启用翻译和批量录音转写，实时语音需另行配置本服务支持的实时语音接口。豆包 WAV 输入须为 16kHz、16-bit PCM、单声道或双声道。

### 后端发布

本项目的后端发布规则：`VERSION` 是版本来源，标签使用 `backend-v0.1.0`，镜像使用 `ghcr.io/metasequoiaime/msime-backend:0.1.0`。首版发布 `VERSION` 中的 `0.1.0`，后续相关代码合入 `main` 后自动递增：`feat:` 增加次版本，普通修复增加补丁版本，`type!:` / `BREAKING CHANGE:` 增加主版本（0.x 阶段增加次版本）。仅 Markdown 文档变化不发布。

工作流先验证主分支源码，再原子推送版本提交和标签，随后构建 Linux amd64 / arm64 镜像，最后创建 GitHub Release。镜像携带源码地址、提交 SHA 和版本 OCI 标签，供 yldm-platform 的 ImageUpdater 跟踪。镜像构建直接依赖同一工作流的发布任务，不依赖默认 `GITHUB_TOKEN` 推送标签触发第二个工作流。

仓库需允许 Actions 使用 `contents: write` 和 `packages: write`；分支规则也必须允许发布机器人推送仅修改 VERSION 的提交，否则工作流会明确失败，不会强推绕过规则。首次 GHCR 镜像发布后，在包设置中将其设为 Public，并验证匿名拉取；公开源码仓库不会自动保证包也是公开的。若选择私有包，则给部署配置有读取权限的 imagePullSecret。

在 Actions 中手动运行“后端版本与镜像发布”可恢复失败发布：没有待发布变更时复用当前版本标签，重新构建镜像并补建缺失的 Release。如果验证期间主分支前进，原子推送会失败；重跑工作流会检出并验证最新 main。若修复发布故障本身引入相关代码变更，则正常发布下一版本。流程不会覆盖已有 Git 标签。

本流程不直接部署 Kubernetes，也不包含供应商 API Key 或客户端令牌。首次镜像成功发布并确认拉取权限后，再启用 yldm-platform 中的后端 ApplicationSet 条目。

## 用户登录与实时语音

用户体系支持 Apple、Google、微信网站扫码、阿里云短信及 Lark 邮箱验证码，使用 PostgreSQL 存储用户和会话。配置、迁移和客户端流程见 [用户体系](docs/user-auth.md)，接口说明可在本地显式启用 `/swagger/` 后查看。

EveryAPI 合作服务实时语音配置：

```json
{"streaming":{"provider":"everyapi","url":"wss://api.everyapi.ai/v1/audio/stream","token_env":"MSIME_EVERYAPI_TOKEN","model":"volc.seedasr.sauc.duration","max_seconds":120}}
```

客户端使用本服务的访问令牌连接 `/v1/audio/stream`，发送豆包 ASR v1 二进制配置和音频帧。Swagger 不提供 WebSocket 音频上传。已通过真实音频验证中间结果与最终识别结果；可用 `MSIME_LIVE_STREAM_TOKEN` 和 `MSIME_LIVE_STREAM_PCM` 环境变量显式运行 `TestPartnerStreamingLive`（16 kHz、单声道、16 bit PCM，最多 10 秒）。

### 原生资源容器验证

镜像构建时按 `third_party/msime` 子模块里客户端的词库锁文件下载并校验发布资源，资源只读放在 `/usr/share/msime`，引擎进程（`msime-backend-engine`）为 `/usr/local/bin/msime-engine`。部署配置的 `engine.binary` 和 `engine.resources` 分别指向这两个路径；生产文档继续关闭。用户词库查询和恢复需要可写 `/tmp`，部署时应提供独立临时卷。

构建后可运行 `python3 scripts/smoke_container.py --image msime-backend-shared-test --native`，在只读根文件系统、2 CPU / 2 GiB 限制下验证内置资源、转换、注音及四路并发日语查询。该检查不替代生产数据库和代理链路验收。

## 官网词条提交

官网表单（msime-web#213）匿名调用 `GET /v1/community/word-submissions`（返回 `{enabled, site_key}`）和 `POST /v1/community/word-submissions`（`{kind, entries, note, token}`）。`kind` 省略时为 `words`，另有 `english` 和 `translations`，其他值返回 `invalid_kind`。服务端校验 1–20 个条目和不超过 500 字的备注：`words` 的条目为 `{word,pinyin}`（词语为 1–16 个汉字或〇，拼音为小写全拼、`'` 分隔、音节数等于字数、音节取自与官网相同的 402 音节表，ü 写作 v、lüe/nüe 写作 lve/nve）；`english` 的条目为 `{word,display}`（word 为最多 64 个小写字母 a–z，display 为最多 64 字符的显示词形）；`translations` 的条目为 `{source,gloss}`（原词最多 64 字符且不以 `#` 开头，译文最多 200 字符）。display、source、gloss 去掉首尾空白，不得含制表符、换行或零宽等控制与格式字符。Turnstile 校验之前先过一道按客户端地址每分钟 10 次的闸门（超限 429 `rate_limit_exceeded`、`Retry-After: 60`），免得脚本让服务端无限次调用 siteverify；它和下面的提交限流一样记在 PostgreSQL 的 `auth_rates` 表（作用域 `word-submissions`），所有副本共享。再做 Cloudflare Turnstile 服务端校验（action `words`，hostname 必须属于 `allowed_origins`）和按客户端地址的 PostgreSQL 限流（每 10 分钟 3 次、每天 20 次，复用 `auth_rates` 表，只存地址摘要）。限流计数失败返回 503 `rate_limit_unavailable`。通过后用专用 GitHub App 追加到 msime-dictionary：`words` 写 `词语<TAB>拼音<TAB>权重` 到 `custom/words.txt`，权重取基础词库（Engine 的 `pinyin_weight_medians`）同音节数词条的权重中位数（8 及以上音节合并），再限制在 words.txt 现有权重范围内以满足 check-words 门禁；Engine 不可用时用 5000，失败不缓存。`english` 写 `单词<TAB>显示词形<TAB>1` 到 `custom/english.txt`，先经 `listed_english_batch` 排除 english.db 已有的同一对。`translations` 写 `原词<TAB>译文` 到 `custom/translations.txt`，同一原词的后一行覆盖前一行，只拒绝完全相同的一对。各类型还会拒绝分支文件中已有的相同条目（`already_listed`）。三类共用一个滚动 Pull Request：有开启的 `community-words/*` Pull Request 就追加提交，否则新建 `community-words/<UTC 时间>` 分支并开 PR；标题按分支相对主分支在三个文件中新增的行数汇总，例如 `feat(custom): add 3 words, 1 English word and 2 translations`。写入 GitHub 的整段读改写（查找开启的 PR、读文件、建分支、提交、开 PR 或改标题）在同一进程内由互斥锁串行，在副本之间由按目标仓库加的 PostgreSQL 会话级 advisory 锁串行，所以多副本同时收到提交时只会开一个滚动 PR；锁只在写 GitHub 期间占用一个数据库连接，等待时不占连接；持锁连接设置了服务端 TCP keepalive，持锁副本所在节点失联时 PostgreSQL 约 1 分钟后结束该会话并释放锁；最多等 15 秒，等不到或数据库无法加锁时返回 503 `server_busy` 并带 `Retry-After: 30`，此时 GitHub 上没有任何写入。文件 blob SHA 冲突返回 409，写入结果未知返回 502 `uncertain:true`，服务端从不自动重试写入，也不删除或强推。词条、备注和令牌不写日志。

需要用户体系（PostgreSQL）和包含官网的 `allowed_origins`（例如 `https://msime.app`）。`turnstile.site_key` 为空时功能关闭；填写后其余字段缺一不可，否则服务拒绝启动：

```json
{"client_ip_header":"CF-Connecting-IP","word_submissions":{"turnstile":{"site_key":"0x4AAAA...","secret_env":"MSIME_TURNSTILE_SECRET"},"github":{"app_id":123456,"installation_id":7890123,"private_key_env":"MSIME_WORDS_GITHUB_APP_KEY","repository":"metasequoiaime/msime-dictionary","branch":"main"}}}
```

GitHub App 只安装到 msime-dictionary，仓库权限只给 Contents: Read and write 与 Pull requests: Read and write（Metadata 只读为默认）；服务端签发的安装令牌再次限定到该仓库和这两项权限。私钥（PEM，PKCS#1 或 PKCS#8，可用 `\n` 表示换行）通过 `private_key_env` 注入。顶层 `client_ip_header` 为空时只信任 TCP 对端；部署在反向代理后必须填写由代理覆盖写入的头（`CF-Connecting-IP`、`X-Real-IP`，或取最后一段的 `X-Forwarded-For`），否则所有访客共享同一份额度。客户端能自带的头不要填。这个设置同时用于账号和社区接口、匿名开户、遥测、后台登录和词条投稿的按地址限额；旧的 `word_submissions.client_ip_header` 写法仍然有效。

## QQ 群反馈机器人

群友在 QQ 群里 @机器人 问水杉输入法的使用问题，机器人按知识库回答，答不上时告诉群友已经记下、管理员会回复（QQ 群文本消息里的 `<qqbot-at-user id="…" />` 实测原样显示成文字，所以回复不 @ 人）。管理员 @机器人 发「提issue 内容」，或者在消息里明确要求记录（「提个 issue」「记一下」「记录一下」「反馈给开发」「建个 issue」等）时，服务端把具体的 bug 或需求整理成 GitHub issue，再在群里回复编号，并提示之后 @机器人 发「#编号」可以查进度；文档里已经有的功能、文档说明是正常的现象不提交，回复文档的说法。管理员没有要求记录的具体反馈只回复机器人理解的一句话概括，提示要记录就用「提issue」；闲聊、玩笑、表态、提问和笼统的愿望不论谁发的都只当问题回答。普通群友的消息都只当问题回答（曾经管理员的每条消息都先自动判断、像反馈就提交，群里讨论和吐槽也被提成了 issue，例如 msime #7153）。

命令不经过模型，直接查 GitHub 或给固定文字：「帮助」列出用法（管理员另外看到提 issue、看和提交或忽略草稿、看文档缺口和差评并写进 FAQ 或忽略、记录答案、周报、自测和改进依据的用法），新人常问的「怎么用」「使用说明」「你能做什么」「有什么功能」「指令」「?」等也回帮助，不交给模型（模型手上没有功能清单，曾把「怎么用」判成答不上、记成假的文档缺口）；只认整句的命令（帮助、版本、我的ID、草稿、文档缺口、周报、自测、改进依据、差评）比较前先把全角字符折成半角、去掉空白和开头的「/」、转小写，再去掉句末的问号、感叹号、句号、波浪号、省略号和表情，所以「帮助？」「/help」「版本！」「最新版本🙏」「我的id？」都认；「版本」列出各平台最新版本，每个平台一行，写成「Windows：0.10.1（10-11）｜新版 Windows 预览 0.2.1（10-08，测试中）」：版本号去掉 tag 前缀，日期只写月和日，不写仓库名；「查 关键词」（「查」「搜」「搜索」后面要跟空格、冒号或逗号，免得「查不到词库怎么办」这类提问被当成命令；「提issue」「提文档」「文档反馈」「记录答案」这类命令后面可以直接接内容；关键词里带「怎么」「为什么」「吗」「？」这类问句说法时是在提问，交给答疑，「查 为什么打不了字」不会被当成搜索）搜相关 issue，回复最多列 5 条，每条一行「msime #7066 标题（还没解决）」，标题去掉「[Bug]」「[Feature]:」这类模板前缀，不带标签、日期和开发者评论，最后一行提示想看某条的进展就 @机器人 发「#编号」（曾经把交给模型的资料原样发进群里，最多十行、带英文标签和几百字没清洗的评论，在千人群里刷屏）；GitHub 出错或限流时回复「GitHub 这会儿查不了」，不说没找到；没搜到时提示想问怎么用可以直接 @ 机器人问、不用加「查」；「#编号」「仓库名 #编号」或「owner/仓库名 #编号」（全角的「＃８１８」也认）查 issue 的标题和状态，只能查答疑用的仓库；GitHub 回 404 或 410 才说没有查到这个编号，拿不到令牌、网络出错、限流、其他错误状态时回复「暂时查不了 #编号（GitHub 出错或限流）」，这句话也是交给答疑模型的资料，模型不会据此说没有这个 issue；「我的ID」见下文；「提issue 内容」只有管理员能用；「提文档 内容」（或「文档反馈 内容」）反馈文档缺了什么、哪里写错了：模型整理成要补充或更正的内容、建议写法和涉及的文档，管理员的反馈先在 `qq_bot.knowledge_repo` 里查重，重复时在原 issue 下补充，否则新开标题以「[文档]:」开头的 issue，只打仓库已有的标签（`documentation` 和 `qq_bot.labels` 里仓库有的）；普通群友的反馈不单独开 issue，以「群友反馈文档：」追加到答不上问题的汇总 issue（需要 `gaps_repo`），免得刷出一堆 issue；每个群友每天最多记 3 条（`auth_rates` 作用域 `qq-bot-docs-member`，按「群/成员」计），整理之后、写进汇总 issue 之前才扣名额，没看出是文档反馈的消息不占，没记下时退回，超了回复「今天已经记了几条了，明天再提，或者请管理员用「提文档」提交」（汇总 issue 每天只有 50 条额度，没有按人的上限时一个人就能占满）。「提文档」前面带一层「帮我」「请」「麻烦」「我想」这类客气话也认；后面开头就是问句（「提文档 怎么用」「提文档在哪」），或者紧接命令词、和它连成一句话（「提文档的格式是什么」）时是在问这个命令怎么用，交给答疑；只看开头，文档反馈本来就常写问句，「提文档 FAQ 没写鸿蒙版怎么下载」照常是命令。「提文档」是命令，先于上面「提issue」的说法判断：内容里写了「记一下」「提个 issue」也只走文档反馈，不会整理成 bug 或需求提到代码仓库。官方机器人只能收到 @它 的消息（`GROUP_AT_MESSAGE_CREATE`），收不到群里的其他聊天，不会旁听。

管理员是群主、群管理员，以及 `qq_bot.admins`（member_openid）里列出的人。群主和群管理员只在 `qq_bot.groups` 列出的群里算数：没有列出群（或者没配置 groups）时只认名单，否则机器人公开后，任何人自己建个群、把机器人拉进去当群主就能「提issue」；这时每个群记一条带 `group_openid` 的日志（`group roles ignored`），方便把要信任的群填进配置。身份先看 @ 消息事件 `author.member_role`，事件没带时用开放平台的「获取群成员信息」接口（`GET /v2/groups/{group_openid}/members/{member_openid}` 的 `member_role`）识别，结果在每个副本缓存 10 分钟，接口临时出错时沿用过期的缓存（管理员不会因为接口抖一下就突然不能「提issue」），从没查到过才按普通成员处理；这个接口还在内邀阶段，没进白名单时返回错误码 11253，服务端此后一小时内不再调用、只认名单，白名单开通后不用重新部署就会生效（2026-10-11 实测本机器人返回 11253）。任何人 @机器人 发「我的ID」，机器人会回复他的 member_openid 和机器人认定的身份，名单就靠这个收集。名单里的人和接口识别出的本群管理员现在只用来判断谁能提 issue，回复里不 @ 人。

QQ 开放平台以 Webhook 把事件推到 `POST /v1/community/qq-bot/events`。服务端用机器人 AppSecret 派生的 Ed25519 密钥校验 `X-Signature-Ed25519` 签名，按消息 id 在 `auth_rates` 里认领（7 天内同一 id 只处理一次，开放平台重推的同一事件直接应答成功），然后立即应答，在后台继续处理：

1. 去掉 @ 标记和表情标签；没有文字时回复用法，超过 2000 字请群友精简。**看截图**（`qq_vision.go`）：消息里有图片时，先让 `qq_bot.model`（claude-haiku-5-5，能看图）把截图转成文字描述——哪个界面、什么系统、截图里的原文（报错、版本号、设置项）、能看出的异常，再接在问题后面走答疑；只发了截图的也照样回答，看图失败时才请群友把文字打出来；文字带截图、截图又没看成时在问题后面注明，由回答的模型在答案里请群友把报错文字打出来，不另占一次回复。管理员「提issue」带截图时，描述并进整理反馈的输入，截图本身照旧存进公开 bucket。命令（「查」「#编号」等）不看图。每条消息最多看 2 张、每张不超过 5MB、40 秒，两张同时看、描述按截图原来的顺序拼（某张没看成时不再补看第 3 张）；截图只在内存里转一次，不保存。不走备用模型：实测 gpt-5.6-terra 连颜色都认错。每个普通群友 24 小时内最多处理 20 条、每个群 300 条，每个管理员 100 条（`auth_rates` 作用域 `qq-bot-member`、`qq-bot-group`、`qq-bot-admin`，窗口从这段时间的第一条消息算起），问答、闲聊和追问都计数，因为它们同样要交给模型。不调模型的命令（帮助、版本、草稿列表、#编号）排在这几个限额之前、不占它们的额度，群额度用完后也能用，只另按人计每 24 小时 60 次（作用域 `qq-bot-free`，群友和管理员都算）防刷；加词、提文档、查、提issue 仍然要过限额。普通群友先查个人限额，通过了才计入群的额度，超了个人限额的消息不占群的额度；管理员不受个人和群限额限制，群额度用完时也能提 issue。个人超限和群超限的回复分开写，命中限额时记一条 `limited` 日志（只有作用域）；回复按窗口还剩多久说什么时候能再问：不到 1 小时说「大约一小时内」，否则说「大约 N 小时后」（N 向上取整），曾经一律说「过几个小时再来吧」，窗口剩余可能接近 24 小时，群友过几个小时来试还是被拦。机器人自己出错（`failed`、`timeout`、`reply_failed`，管理员的 `admin_failed` 也算）或者被副本关停打断时，退回这条消息已经计上的个人、群和管理员额度，超限的那一项本来就没计上；回复会请群友再发一次，不退的话每重发一次就再扣一次，上游一抖或赶上发布时群额度白白耗掉。同一个群友（群 + member_openid）的消息按到达顺序一条一条处理：副本内每个群友排一个队，跨副本用 `auth_rates` 的 `qq-bot-busy` 当 90 秒租约，两步合计最多等 30 秒，等满了、数据库不可用时照常并行处理；原来每条消息并行处理，「候选框跑到左上角了」隔几秒再 @ 一句「win11 0.9.5」，后一句读不到前一句，两条各答各的，还会起两份草稿。处理一条消息最多 4 分钟，这是各步共用的预算：模型调用、现查和重答都按剩下的时间收缩（见下文的备用模型和现查），剩余不到 90 秒不再现查，现查之后剩余不到 25 秒不再重答、按答不上处理，跳过时记一条 `budget exhausted` 日志（`stage` 是 `research`、`reanswer` 或 `fallback`）。副本关停时先不再接新消息（应答失败，开放平台重推给其他副本），等进行中的消息处理完、最多 75 秒，再取消剩下的处理，所以部署的 `terminationGracePeriodSeconds` 要配成 100 秒，盖住 preStop、HTTP 关停、这 75 秒和最后的指标刷写；曾经一进来就取消，每次滚动发布都把正在答的消息砍掉。对话历史、文档候选、草稿、汇总 issue 这些收尾写入也不跟随处理的 ctx，另限几秒，答案已经发出去的那一轮照样记下，下一句「还是不行」接得上前文。回复不跟随处理的 ctx，处理超时或副本滚动更新时兜底回复照样发出：等满 75 秒还没处理完、被更新打断的说「机器人正在更新，请一分钟后再 @我 发一次」，4 分钟用完（或剩余不到 20 秒、来不及换备用模型）时说「这个问题查了太久没查完。可以说具体一点（哪个系统、哪个功能、看到了什么）再 @我。」，处理结果记为 `timeout`，其他失败才说「暂时处理不了」。**斜杠命令**（`qq_slash.go`）：管理员提议的一套短写法，和 QQ 输入框里打「/」弹出指令列表的习惯一致。`/help`（`/h`）、`/ver`、`/weekly`、`/drafts [编号]`（也认打错的 `/darfts`）、`/submit 编号 [补充]`、`/undraft 编号`、`/draft 编号 内容`、`/issue [内容]`、`/faqs`、`/a 编号 [答案]`、`/doc [内容]` 改写成对应的中文命令（帮助、版本、周报、草稿、看 / 提交 / 忽略 / 补充 D编号、提issue、文档缺口、写进FAQ F编号 答：…、提文档）再处理，权限、限额和回复照旧；`/draft`、`/issue`、`/q`、`/doc` 后面没写内容时用引用的那条消息（见引用的消息），写了内容也把引用的那条接在后面，`/a 编号` 没写答案时用引用的那条当答案。原来没有的几样：`/draft [内容]` 让管理员按引用的群友消息新建草稿（整理成 bug 或需求、写上还缺什么，不提交；看不出是 bug 还是需求时不建）；`/q` 把引用的问题记成文档缺口（待补答案的问题），之后 `/a 编号 答案` 写进 FAQ；`/issues` 不带参数列各仓库最新开着的 issue（按创建时间），`/issues 编号` 查编号，第一个词是答疑仓库的名字或简称（`win`、`windows` 是 msime-windows，`msime`、`mac`、`linux`、`android`、`ios` 是 msime，`docs` 是 msime-docs）时只在那个仓库查编号或搜关键词，其余当关键词搜；`/docs` 回文档地址。管理员命令群友发时回「只有管理员能用 /命令」。不认识的命令只有整句是「/单词」时才回斜杠命令的用法，「/Library/Input Methods 里没有」这类以路径开头的提问照常答疑。`/help`、`/ver`、`/docs`、`/drafts` 和中文的帮助、版本一样只计宽松额度。**机器人自己的编号**：群友收到「已记下（D3）」后常来问「D3 是什么」「D3 怎么样了」，机器人曾经不认得自己的编号，去 GitHub 搜了一圈说「D3 不像水杉里的功能名」。现在问题里提到 D编号（前后不紧挨字母和数字，「3D」「D3D11」不算）时，资料最前面加「机器人自己的编号」一节，写上这份记录报的是什么（模型整理的标题，不含原话）、哪天记的、现在是等管理员确认、已提交成哪个 issue、管理员看过没转成 issue 还是已经过期，最多 3 个（`qqOwnCodes`）；答题和挑段落的提示词也说明 D、F、B 加数字是机器人自己的编号，不去 GitHub 搜。**引用的消息**（`qq_quote.go`）：群友常常引用一条带截图或问题的消息再 @机器人 问「这个是什么意思」，事件里 `msg_elements` 带着被引用消息的文字和附件时，截图并进附件（自己没带截图时）交给看图和提 issue，文字在命令之后以「（群友引用的消息：「…」）」接在问题后面（最多 500 字），只 @ 不写话时引用的话就是问题；只有 `message_type`=103 或 `message_reference` 对象、拿不到内容时，在问题后面注明看不到引用的内容，回答的模型先按输入法里最可能的意思答，再请群友把截图和问题放在同一条消息里。这几个字段的位置生产上还没确认，单独解析，类型对不上就当没有引用，不影响消息本身；`qq bot: event fields probe` 每种新的字段组合记一行（顶层字段名、数字字段、`message_reference`、`msg_elements`、`message_scene` 下一层的字段名，不记内容，每个副本最多 32 种），用来确认实际字段。曾经引用了截图问「括号里的 AD 什么意思」，机器人看不到截图，答成了广告、公元；答题和挑段落的提示词也改成含糊的说法和字母先按输入法里的意思理解。群里 @机器人 再打「/」会弹出指令面板：开放平台控制台没有配置面板的表单，面板用指令面板接口管理，内容在 `scripts/qq_command_panel.json`（群友的命令、使用文档和下载两个链接，管理员的命令标 `only_admin`，只有群管理员点得了），改了斜杠命令后在部署完新版本之后运行 `QQ_BOT_SECRET=… scripts/qq_command_panel.py` 同步（有就修改、没有就新建，`--dry-run` 只看当前的面板）。有了面板，「帮助」只列斜杠命令，每条一行带一个例子，群友看到的不超过十行、管理员的不超过二十行（原来每个中文命令一段 @水杉助手 示例，群友的二十多行、管理员的五十多行），末尾提一句中文命令照旧能用。另外几个只在面板里用得上的：`/add 词语 拼音` 是加词，`/bad`、`/selftest`、`/report` 是差评、自测、改进依据；从面板点 `/submit`、`/undraft`、`/a` 常常没带编号，这时先说一句带例子的写法，再列出草稿或文档缺口，照着编号发就行；`/docs` 同时给文档和下载地址。
2. 普通群友不是命令的消息都按问题回答，见下文「答疑知识库」。管理员不是命令的消息和「提issue」的内容先用 `chat` 端点（JSON 输出）和 `qq_bot.model` 整理成 bug 或功能建议的各个栏目，栏目与 msime 的 `bug_report.yml`、`feature_request.yml` 模板一致。只有具体、开发者能据此着手处理的反馈才算 bug 或需求：说清楚出了什么错的 bug、提出了具体改动的需求；闲聊、玩笑、表态、提问和笼统的愿望都算其他，模型返回的 bug 没有问题描述、需求没有改进建议时服务端也按其他处理。请助手分析原因、给排查或修复建议、评价或写方案的消息，即使描述了 bug 也算其他。其他消息里，「提issue」的请管理员写具体些，不提交；普通消息转去答疑。是 bug 或需求时先看是不是明确要求记录：「提issue」命令一定是；其他消息要整理时模型给出 `intent`，或者去掉空白、转成小写后含有「提issue」「提个issue」「建个issue」「开个issue」「记一下」「记录一下」「帮忙记录」「反馈给开发」等说法（单独的「记录」不算，免得「历史记录丢了」这类 bug 描述被当成要求）。没有要求时照常转去答疑，答疑判断是在报问题时，答案后面接一句「要记成 issue 就 @我 发「提issue」加上描述」，不像群友那样起草稿、记汇总 issue。用「记一下」这类说法要求记录时，先用答疑同样的办法从知识库挑段落（只用文档，不查 GitHub），由模型给出明确的判断：文档说明功能已经有了、现象是设计如此或已经回答了问题时为 `existing`，回复文档的说法并提示「这个办法不管用就用「提issue」说一下哪里不行」，不提交；其他情况为 `needs_issue`。这一步失败（知识库拉不到、模型输出不合格式）时照常往下走。「提issue」命令是管理员已经判断过要提，不做这一步对照。整理这一步失败时，没要求记录的消息退回去直接答疑，明确要求记录的回复「没能整理成 issue，请把问题再描述一遍后 @我 发「提issue」。」内容里写的「优先级 高/中/低/紧急」或「P0–P3」、「effort/工作量 XS–XL」会先取出来：目标仓库有 `priority/p1`、`size/s` 这样的对应标签时打上，没有时写进正文的「维护者标注」，不新建标签。bug 既看不出平台、也没有操作步骤时，回复一句追问，不提交。这条规则由服务端判定，模型没给出追问时用固定的问题。追问（包括下文 Windows 问版本）时先把反馈存成草稿（见下文「群友报 bug 整理成草稿」，草稿属于这位管理员），回复「还差：…。已存为 D12，补一句就行：@水杉助手 提交 D12 版本 0.9.5」，不用把整段描述重打一遍；内容里写的优先级、工作量不存进草稿，写进这句示例，照着发不会丢；截图和视频不跟着草稿提交，有附件时一并说明。没有草稿存储或存不进去时，照旧提示补充后用「提issue」把问题完整描述一次：管理员第一次用「记一下」这类说法时，补充的那条消息往往不会再说一遍，没有命令就只会得到一句话概括。
3. 按平台选仓库，规则与崩溃 issue 相同：能看出平台时用 `admin.github.platforms` 里该平台的 `repo`（生产上 Windows 是 msime-windows，其余平台是 msime），看不出平台时用 `qq_bot.repo`。Windows 上有两个产品：msime-windows（Metasequoia IME 水杉输入法，现在的正式版）按上面的平台配置提交；msime 仓库里的新实现提交到 `qq_bot.repo` 并加 `platform/windows`。两边的版本号会重叠，所以不写死分界，按两边的 GitHub Release 实时判断：新实现的范围从 0.1 到 `qq_bot.repo` 里最新的 `windows-v` 版本再往后一个 minor（覆盖还没发布的预览构建），msime 的 Windows 升版本不用改代码；范围外的版本归 msime-windows。版本号落在新实现的范围里、又不低于 msime-windows 当前在发的 minor 时，两个产品都可能，先追问安装包名（新实现按方案分成 `MetasequoiaIME-Full_Setup`、`MetasequoiaIME-Pinyin_Setup` 等，msime-windows 只有 `MetasequoiaIME_Setup`）；消息里写了这类安装包名或「拼音版」「五笔版」等版本名时直接按它分。更早的重叠（两边都发过的 0.1 到 0.3）按仍在发布的新实现处理。Release 查不到时退回 0.1 到 0.3。没说版本时先追问版本，不提交；管理员在「提issue」或「提交 D12」里写「版本 未知」（也认「版本：不知道」「版本 不清楚」）时不再追问，按平台配置提交到 msime-windows，正文的「系统与输入法版本」写明输入法版本未知。这条只对管理员生效，群友补充「版本 未知」照样追问。曾经写死成 0.9 以下、后来写死成 0.1 到 0.3，前者把 msime-windows 0.0.x 和 0.4 到 0.8 的老用户提错了仓库，后者会在新实现升到 0.4 后出错。用模型给出的关键词在这个仓库里搜索开启的 issue（关键词去掉冒号和引号，写不进 `repo:`、`is:` 这类限定词），交给模型判断是否重复。重复时在那个 issue 下补充评论，否则新建 issue，标签为 `bug` 或 `enhancement`、该平台的 `label`（为空时不加）、`qq_bot.labels`，以及模型从目标仓库已有标签里挑的 0 到 2 个分类标签（类型、状态、自动化和 `platform/`、`size/`、`priority/` 这类流程标签不给它挑，挑不准就不打）。
4. 管理员消息里的图片和视频一并带上：GitHub 的 issue 接口不能上传附件，所以服务端从 QQ 下载附件（只访问 QQ 的 CDN 主机，重定向也一样），按文件头核实类型，只收 PNG、JPEG、GIF、WebP 图片和 MP4、MOV、WebM 视频（图片不超过 20 MB，视频不超过 50 MB，一条消息最多 9 个），写进 `auth.avatars` 的公开 bucket（前缀 `qq-feedback/`，对象名随机），再把链接写进 issue 正文或补充评论的「截图与视频」一节：图片直接显示，视频给链接。存储时的 Content-Type 用服务端认出的类型，不用 QQ 给的，所以 HTML、SVG 之类的内容不会出现在公开域名上。存不上的附件不影响提交，正文注明「另有 N 个附件没能保存」，提交成功的回复里也说明有几个截图或视频没能附上；语音不收，普通群友的附件也不处理。没有配置 `auth.avatars` 时图片和视频都算没能保存。附件是原图原视频、不做脱敏，管理员常把群友的截图转发过来提交，所以至少附上了一个附件时如实说明：「提issue」带截图时一进来回的「收到」后面加一句「截图会公开，转发群友截图前先打码。」，「截图与视频」一节开头注明「附件为原图，未做脱敏。」，页脚改成「文字部分不包含反馈者的原话和身份」，提交成功的回复把「不包含你的昵称和原话」换成「截图和视频原样附在 issue 里，里面如果有昵称、QQ 号等个人信息，请在 issue 里删掉对应的图片。」；一个附件都没存上时文案照旧。
5. 被动回复群友 issue 编号。开放平台只允许消息里出现报备过的域名，所以回复不带链接。被动回复只能在收到消息后 5 分钟内发出，后台处理限时 4 分钟。

### 答疑知识库

知识库是 msime-docs 的用户指南，与官网 FAQ 页同一份来源：`qq_bot.knowledge_repo`（默认 `metasequoiaime/msime-docs`）的 `qq_bot.knowledge_dir`（默认 `guides`）目录下的 Markdown 文件，不含子目录，所以 `zh-TW` 译本不收。补充或修改问答就给 msime-docs 提 PR，合并后最多 10 分钟机器人就用上（每个副本最多每 10 分钟刷新一次，读请求带 ETag 缓存，刷新失败时继续用已加载的版本；副本启动时先预热一次，不等第一条答疑才去拉）。答疑时知识库读不到或挑段落的模型出错，不再直接回「暂时处理不了」：带着空段落照常答疑，照常查提到的 issue、搜 issue，资料开头注明文档暂时读不到，回答用 `strong_model`，水杉本身的事实答不上就按答不上处理，常识和闲聊照常回答；处理时间用完时仍按出错处理，提 issue 前的对照读不到文档时照旧跳过对照、照常提交。服务端用管理后台的 GitHub App 只读拉取（安装令牌只有 contents 和 metadata 读权限），按标题把文档切成段落，标题链形如「安装 > 卸载」。

答不上的问题汇总给维护者：设置了 `qq_bot.gaps_repo`（例如 `metasequoiaime/msime-docs`）时，文档和 GitHub 里都找不到答案的问题，由回答的模型用一句话概括（不含人名、QQ 号、链接，@、# 和链接再经 `githubReferenceBreaker` 打断），写之前删掉手机号（可带 +86）、邮箱，以及「QQ」「群」「微信」后面跟着的号码，换成「（已隐去）」（在去重和截断之前做，7 天去重的键和写进 issue 的是同一段文字；微信号只在写明「微信号」或带冒号时才删，「微信 windows版」这类说法不动），作为评论追加到这个仓库里标题为「QQ 机器人答不上的问题」、由机器人自己建的开放 issue（`performed_via_github_app` 是管理后台的 App，或者作者是 App 的 bot 账号「<slug>[bot]」，slug 启动时用 App 的 JWT 调 `GET /app` 取一次并缓存，没取到时下次用到再取；外人新开的同名 issue 不算，曾经这样就能把答不上的问题都引到别人的 issue 下；分不清同名 issue 是谁建的时这次不记，不新建，免得重复）；没有这个 issue 时新建（持跨副本锁，免得两个副本各建一个），编号在每个副本缓存 1 小时，关闭它后新的问题会记到新开的 issue。同一个概括 7 天内只记一次，每天最多 50 条（`auth_rates` 作用域 `qq-bot-gap`、`qq-bot-gaps-day`）；先查每天的额度再占 7 天去重的名额，当天额度用完时不会把这个问题的去重名额也占掉；去重名额占了之后取凭证、找汇总 issue 或写评论失败时退回（`ReleaseRateLimit`，每天的额度不退），回复如实说没记下，下次遇到同一个问题重新写，不会被说成「已经记下来了」。没有记下时（没配置、概括为空、额度用完）记一条 `gap skipped` 日志，原因是 `no_repo`、`empty_topic` 或 `daily_cap`。评论按提问的人写「群友问：」或「管理员问：」（管理员自己试机器人的问题原来也写成「群友问」，维护者分不清哪些是群友真的问过的）。答上了、但模型说有一部分资料里没有（回答里的 `gap` 字段）的问题也记进来，开头写「群友问（答得不全）：」或「管理员问（答得不全）：」，记的是模型说的缺口；现查过的不记，它已经记成了文档候选（见下文「把答案沉淀进文档」）。回答提示要求不确定的地方半句话带过，答不上只用于完全没有资料，答得含糊的问题原来哪里都不记。维护者照着补 msime-docs，机器人能答的就越来越多。为空时不记录。`gaps_repo` 是新增的配置字段，要等所有副本都运行支持它的版本之后再写进配置，GitHub App 要有这个仓库的 Issues 写权限。

改进依据汇总给 msime-cloud：答不上的问题不全是文档缺口，很多是机器人自己的问题（文档写了却没检索到、工具查不到、超时），该拿来改这个仓库的代码。答不上时除了上面的 `gaps_repo`，还在 `qq_improvement_events` 表记一条，只存回答模型概括的话题（不存群友原话），并分类：现查过 GitHub 或网上还答不上的算「机器人能力问题」，没现查就答不上的算「文档没写」；管理员「自测」也记下第一句分数（不记机器人的回答）。每周一 9 点（北京时间）以后，后台任务（每小时看一次，用 `auth_rates` 作用域 `qq-improvement-report`、按 ISO 周认领，几个副本只有一个开，认领后开失败也不重试）在 `qq_bot.improvement_repo`（为空时是 `metasequoiaime/msime-cloud`，写 `none` 关闭）开一个标题为「QQ 机器人改进依据：MM-DD ~ MM-DD」的 issue，汇总上周（周一到周日）的：概况（群友和管理员分开的各类结果次数和答上率，出错和其中的超时、回复没发出去、限流，被说答错的次数，最近一次自测分数；数字来自 `qq_daily_stats`，和下面几节是同一段日期），「机器人能力问题」和「文档没写」两节按话题（忽略空白和大小写）归并、按次数排序，「被说答错」一节由模型把每条差评概括成一句改进线索（不写人名、号码、链接，不照抄原话；概括失败时只写条数，让管理员在群里看「差评」）；每节最多 30 条，每条带勾选框，话题经 `githubReferenceBreaker` 打断 @ 和 #、去掉链接；仓库已有 `qq-bot` 标签时打上。管理员发「改进依据」马上汇总最近 7 天（含今天）开一份（`qq-bot-improvement`，每人每天 2 次）。这段时间里没有答不上、被说答错、出错和回复没发出去时不开 issue，命令回一句「最近 7 天没有……没开 issue」，周任务只记日志：曾经什么都没有也开了一份（msime-cloud #150），四节全是「没有」；而且命令算到当天 0 点为止，当天的问题反而不在里面，概况又统计到了当天，两处的数字对不上。记录保留 60 天。GitHub App 要有这个仓库的 Issues 写权限。

**文档缺口自动起草 FAQ PR**（`qq_docs_drafts.go`）：每周一开改进依据之前，挑上一周「文档没写」里出现至少 2 次的话题（按出现次数，最多 3 个），让机器人强制现查文档和 GitHub 再答一遍；答得上（不是答不上、不是在报问题）就走「记录答案」同一套流程（`writeQQFAQ`）在 `qq_bot.knowledge_repo` 开 FAQ PR，PR 正文和提交说明写明答案是机器人起草的、没有经过人工核对，也不记成自测题；开出的 PR 列在当周改进依据的「文档没写」一节后面。同一个话题起草过 60 天内不再起草（`qq-bot-docs-draft`）；现查也答不上、或者开 PR 失败时退回名额，下周再试。原来文档缺口只记进汇总 issue，要有人照着写文档，一直没人写，机器人能答的也就一直没变多。

追问能接上前文：每位群友在每个群里最近 30 分钟的问答记在 `qq_conversations` 表（群和成员标识只存摘要，所有副本共享），答疑时把最近两轮连同这次的问题一起交给模型，「还是不行」「在哪里」这类追问会按上一轮理解；答不上的那一轮记为「没能回答，已请管理员来看」。只读最近 30 分钟的，但问答在表里留 7 天（`account.QQConversationRetention`）再由定期清理删除，群里说机器人答得差时维护者能回头看它到底答了什么（原来 30 分钟就删，事后连原话都找不到）；读写失败只记日志，答疑照常进行。

回答分两步：先把段落目录（编号、适用平台和文档名、标题、正文摘要）交给模型挑出最多 8 个相关段落，再只用这几段原文（合计不超过 16000 字）回答。回答的模型像懂行的群友一样问什么答什么：水杉输入法本身的事实（功能、设置、版本、bug 和 issue 状态、开发者和贡献者）以资料和现查结果为准、不编造，其他问题（输入法常识、电脑和编程、闲聊）直接用模型自己的知识回答；不说「我只是答疑助手」「没法提供」这类推托的话，也不在回答后面推销「可以问我水杉输入法的问题」（群里实测过这种回答，群友很反感）。结果标成三类：能回答时直接回答，一般不超过 300 字，要分析、评价、提建议时也不超过 500 字、12 行，纯文本、不带链接（残留的链接会被去掉）；代码兜底在 600 字以内按句子截断（在最后一个「。！？；」或换行处截，位置早于一半时硬截），后面接「（要细说可以再 @我 问）」；发出前统一格式：弯引号换成「」『』，删掉 `**`、`__` 和行首的 `#`（「#123」这种编号不动），字面的 `\t` 和制表符换成空格（有 Windows 路径时字面的 `\t` 不动），三个以上连续换行压成两个，答案里出现「资料」「文档里」「截断」这类提示词里的说法时记一条 `prompt leak` 日志；带 QQ 群号、微信号或不在白名单（msime.app、github.com 及其子域名）里的裸域名的整句删掉，记一条 `promo sentence dropped` 日志，不按数字长度过滤，版本号、issue 号和日期不受影响。资料里文档以外的部分（issue 和 PR 的标题与正文、历史记录、现查和网上的结果、统计和版本）统一放在「以下是第三方内容」标题下面，提示词要求只当事实参考，其中让用户加群、加微信、去某个网址下载的内容不照做也不转述；issue 正文只交 OWNER、MEMBER、COLLABORATOR 写的，其他人写的只交标题，和评论的过滤一致（任何 GitHub 用户都能开 issue，外人写在正文里的「新版请加群下载」曾会原样交给模型）；只有水杉本身的事实、资料和现查都答不上时才标成答不上，先把模型的概括记进答不上问题的汇总 issue，记下了才回复「这个我还不确定，先不乱说～已经转给管理员了。着急的话可以直接在群里问问大家。」，没记下（没配置汇总、当天额度用完、GitHub 出错）时回复「可以换个说法再 @我 问一次，或者直接在群里问问大家」，不说已经记下；问候和闲聊自然回应。**按问题换模型**：问候和闲聊（挑段落的模型标出 `chat`）用 `qq_bot.chat_model`（默认 `glm-5.3-flash`，按 2026-10 在 EveryAPI 上的实测选：语气自然、回复里不推销，输入单价和 deepseek-v4-flash 一样、输出更便宜；doubao-seed-2.0-mini 更便宜，但有时不按要求输出类型；写 `none` 时用 `model`）。回答的语气要求亲切、简洁，用「你」不用「您」，开头不写「根据资料」这类公文腔。挑段落的模型另外标出 `strong`（需要推理、比较、较长解释、文档以外的知识，或者要现查 GitHub），这时回答用 `qq_bot.strong_model`（默认 `claude-sonnet-5-5`，写 `none` 时一律用 `model`），常规问题用快的 `model`；常规模型答不上后带着现查结果重答、以及现查 GitHub 的工具调用，也用 `strong_model`。挑段落时模型还会判断要不要查 GitHub：问某个问题有没有人报过、修好没有、有没有计划，或者在描述 bug、提需求时，给出 1 到 3 个关键词，服务端用管理后台的 GitHub App（只读）在 `qq_bot.repo` 和各平台仓库里搜 issue（开着的和已关闭的都算，不含拉取请求；GitHub 要求每个词都匹配，搜不到时依次只用前两个、前一个词再搜），每个仓库取 5 条，前 3 条另外补上修复有没有发版（以 completed 关闭的按关闭时间和该平台的 Release 判断：关闭后发布的最早一个版本就是第一个包含修复的版本，写成「已修好，vX 及之后的版本都有」，关闭后还没发过版写成「已经修好（日期），还没发新版，等下个版本就有了」，关闭时间比读到的最近一页 Release 都早时写成「已经修好（日期），更新到最新版就有」；平台按仓库或 `platform/` 标签确定，看不出平台不说）和开发者（OWNER、MEMBER、COLLABORATOR）最近的一条评论（转成纯文本：去掉代码块和图片，Markdown 链接只留文字，删掉裸链接、加粗和代码的符号、行首的标题、引用和列表标记，丢掉表格行；按句子截到 600 字以内，写成「开发者最近说」：面向群友的角色只用「管理员」和「开发者」），「#编号」查询和提问里提到的编号也一样（直接回复群友时评论截到 150 字）；状态写成还没解决、已完成、不计划处理（开着的不写「开着」，那是 GitHub 的说法，群友关心的是修没修；也不写「还在处理」，开着不等于有人在处理），PR 标成「（代码改动）」，打了 invalid、duplicate、wontfix 标签的写成已关闭（无效、重复、不修）；msime 合进开发分支就关 issue，所以 completed 写「已完成」不写「已解决」，回答的模型也被告知「已完成」不等于新版已经修好。普通提问里提到的编号（例如「#818 修好了吗」，最多 3 个）直接查出状态写进资料，编号前面的词是答疑仓库名时只查那个仓库；用户提到的 issue、搜 issue、仓库统计和最新版本之间没有依赖，同时查，资料仍按原来的顺序拼；搜 issue 时各仓库同时搜，补上下文也同时补。最新版本按各平台的 `tag_prefix` 列出最新正式版和更新的预发布版，平台仓库不是 `qq_bot.repo` 时再列出 `qq_bot.repo` 里「平台 id-v」开头的预览版。各平台最新版本不管问没问都同时查（带 ETag 缓存），文档里写的版本号可能过时，提示词要求版本只按这份清单说；每个平台的最新正式版下面附一行「更新内容：…」（发布说明去掉图片、HTML、代码块，转成纯文本，条目之间用「；」，按句截到 300 字，预发布和预览版不附；附上后资料超长时只给用户提到的平台附），问某个版本更新了什么时挑 3–5 条、每条一行，最后说完整说明在官网下载页。问仓库有多少 issue、PR 这类数量时，模型标出 `stats`，服务端用搜索接口的 `total_count` 列出每个答疑仓库现在开着的 issue、建仓以来累计关闭的 issue、开着的 PR，以及今天和最近 7 天（北京时间）新开和关闭的 issue、合并的 PR（某个数查不到时那个仓库不写，不写成 0；「已关闭」写明是累计数，曾经被群友当成今天处理的数量）。每次统计要打 18 次搜索接口，GitHub 的搜索接口每分钟只给 30 次，所以结果在每个副本缓存 5 分钟，各仓库同时查、每个仓库最多 3 个搜索同时发（不再加大，免得撞上 GitHub 的次级限流），某个数查不到时取消这个仓库剩下的查询；追问之前对话里的数量时也查统计。查询失败的仓库不写进资料（查不到不等于没发布），issue 搜索失败时资料里注明「暂时搜不了，不能据此说没有人反馈过」，答疑照常进行。回答引用 issue 时写仓库名和编号，只能引用查到的。机器人自己的回复（提交、补充到已有 issue、需求计数、提文档、加词和 FAQ 开的 PR）里提到的 issue 也统一写成「仓库短名 #编号（链接）」，例如「msime #12（链接）」，和「#编号」查进度、答疑引用的写法一致，群友看到的同一个 issue 只有一种叫法；链接放在全角括号里，QQ 拒收带链接的消息时连括号一起去掉，剩下「msime #12」。群友在报 bug、描述异常或提需求时，模型另外标出 `escalate`，并用「平台 + 现象」概括问题，服务端以「群友报告问题：」记进同一个汇总 issue（每天另有 50 条额度，作用域 `qq-bot-reports-day`，不挤占文档空缺）；回复先发资料里的排查办法，记下了再单独回一句「这个情况我已经记下来转给管理员了」，没记下时改说「要是还没解决，可以在群里找管理员，请他转给开发者」（提需求时也接这两句，所以不写「如果还不行」）。链接只删掉协议名和之后连续的 ASCII 字符，紧挨着链接的中文保留（曾经用 `\S*://\S*`，中文句子里没有空格，整句会被一起删掉）。

回答一律用 `strong_model`（默认 claude-sonnet-5-5），只有闲聊用 `chat_model`：曾经只有挑段落时标了 `strong` 的难题才用它、其余用 `model`（claude-haiku-5-5），结果「对这个 issue 提点改进意见」这类没被标上的请求只得到复述；群里一天几十个问题，全用 sonnet 的花费可以忽略。`model` 只留给挑段落、整理反馈、看截图、标拼音这类结构化的活。「帮助」的每个命令都带一个能照着发的例子。

**群友报 bug 整理成草稿**（`qq_drafts.go`、`account/qq_drafts.go`）：答疑时模型判断群友在报 bug 或提需求（escalate），机器人先把排查办法发出去，再用管理员提 issue 的同一套整理（`qqSummarise`）把反馈整理成 issue 草稿，存进 `qq_drafts` 表（所有副本共享，30 天后删除），然后单独回一行「已记下（D12），管理员确认后转给开发者。」（对群友不说「草稿」「GitHub」）；缺平台、版本、复现步骤时在同一行接着问「还差：…，比如：…，直接 @我 回一句就行。」，示例按缺的字段生成（缺平台给「安卓 / Windows 11 / macOS」，问到版本给「0.10.1（设置的「关于」页能看到）」），不再写死一个系统和版本让群友照抄；会追问时答案末尾问版本、系统的那句先去掉再发，同一件事不问两遍（答案只多等整理这一次模型调用，查重和建草稿仍在答案发出之后）。群友本人或管理员也可以发「补充 D12 内容」补上（也认「补充：D12 …」「补充 d 12 …」这类写法，「提交」同样），重新整理。群友也可以不带编号、直接回答追问（例如回一句「Windows 11，0.9.5」）：上一轮回复是追问、消息不超过 80 字、不是问句、不是只有「谢谢」「好的」这类客气话或「不对」这类否定（否定照旧走差评重答），而且草稿没提交、是这位群友发起的，就补进那份草稿，不当成新问题去答，也不会再起一份草稿；补充后还缺信息时接着追问，下一句照样能接上；对话历史里改文案之前的追问（「编号 D12…还想知道」）同样认。群友刚起过草稿、30 分钟内又说了一遍（草稿没缺信息、回复里没有追问，上面的直接回答接不上时）：草稿是这位群友的、还没提交，就让模型判断是不是同一件事，是就补进原来那份，不新开一份，也不占新建草稿的额度。整理草稿用的是群友这次和前几轮说的话（不含机器人的回答，从最近一次起草稿、补充或记票之后算起），先问「候选框跑到左上角怎么办」、再说「试了还是不行，win11 0.9.3」时，草稿里带着第一句的现象；草稿说明连同答案记进对话历史，下一轮的模型看得到草稿编号和追问。补充后被整理成其他（看不出是具体的 bug 或需求）时请群友写具体些，草稿不改；整理或数据库出错时回「稍后再试」并记日志，只有草稿已经提交或过期才说改不了。只有管理员能提交：「草稿」列出待处理的草稿（编号、类型、时间、标题、还缺什么），最早的在前，表头写总数，超过 10 份时说明只列了最早的 10 份，有效期剩下不到 3 天的标上「（n 天后过期）」：原来最新的在前，积压多了最早的那些排不到就悄悄过期。「看 D12」回复整理结果的各项（标题、平台、输入法版本、现象、复现步骤、期望、场景、建议）、状态和群友原话的前 300 字，管理员和发起人能看。「忽略 D12」只有管理员能用，把误判、重复的草稿拿掉（`issue_ref` 记成 `ignored`），之后不再列出，补充、提交时回「已忽略」；正在提交的草稿不能忽略。「提交 D12」按平台选仓库、查重后提交，可以顺带补一句（「提交 D12 版本 0.9.5」）或写优先级、工作量。提交前先在数据库里认领草稿，两个副本不会提两遍，提交失败放回；放回和记下 issue 编号用单独的 5 秒时限，不受处理消息的 ctx 影响（提交失败往往正是因为它被取消或超时，例如滚动更新）。认领了还没记下 issue 的草稿回「正在提交，请稍等一两分钟再看」；认领超过 5 分钟（比处理时限 4 分钟长，`account.QQDraftClaimTimeout`）还没记下 issue 的当作提交被打断（例如副本被杀掉、来不及放回），重新出现在列表里，可以补充和再提交，万一真的提了两次，查重会把第二次补充到已有的 issue。每个群友每天最多新建 5 份草稿（`auth_rates` 作用域 `qq-bot-drafts`）；额度用完、整理出错时照旧记进答不上问题的汇总；整理下来不是具体的 bug 或需求（多半是在问用法）时只回答案，不记汇总，也不说已经转给管理员。群友的截图不存进公开 bucket，草稿里只有看图得到的文字描述。

**加词**（`qq_words.go`）：群友发「加词 词语 拼音」（一次最多 5 条，用逗号或换行分隔），机器人走和官网投稿完全相同的流程：校验词语和全拼、敏感词过滤、查词库里是否已有、按词频定权重，写进 msime-dictionary 的同一个滚动 PR，维护者审核合并。拼音可以用空格或 ' 分隔，也可以连写（`shuishanshurufa` 按音节表切成和字数一样多的音节，有多种切法时前面的音节取长的），可以带声调（`kàng bǎ zǐ`，ü 和带声调的 ü 都折成 v），可以用括号或引号括起来（「扛把子（kang ba zi）」）；词语两边的引号、书名号会去掉（「加词「扛把子」」）。没写拼音时让模型按常用读音标上，回复里写明用的读音。校验不过时用 QQ 自己的说法报错（拼音音节之间空一格，不照搬官网表单「用 ' 分隔」的文案，词语只出现一次）。每个群友每天最多提交成功 5 次（`auth_rates` 作用域 `qq-bot-words`），校验、敏感词检查和查重都通过、真要写进词库时才计，写错了改了再发不占这个额度，确定没写进去（PR 里已经有、撞上别人同时提交、GitHub 出错）时退回，确认不了结果时不退；另按发的次数每天最多 20 次（作用域 `qq-bot-words-try`），不管成没成都计，防止反复刷补拼音的模型调用；提交说明里只写「来自 QQ 群」，不记录群友身份。没有启用 `word_submissions` 时引导去官网投稿。前面带一层「帮我」「请」「麻烦」「我想」这类客气话也认（「帮我加个词 扛把子」直接加词）。「加词」后面是在问这个功能时交给答疑，不提交：开头就是问句（「加词 怎么用」）、紧接命令词连成一句话（「加词后不生效怎么办」「加词条怎么用」），或者没写拼音而任何位置出现「怎么」「吗」「吧」「不生效」「？」这类问句说法（「加词扛把子怎么不生效」）；误提交会进公开的词库 PR，所以比提文档判得严。写了拼音的一定当加词，「加词 酒吧 jiu ba」里的「吧」不算问句。词库只收纯汉字：「加词 栓Q shuan q」「加词 打call」这类开头的汉字后面紧跟字母、拆出来的拼音又校验不过的，直接回复加词只收纯汉字、带字母的词可以在输入法设置里加成快捷短语，不再把词语截成一个字报拼音错误；拼音连着词语写（「秋天qiutian」）照常能加。群友说某个词打不出来时，答疑会顺带教他用「加词」（只对纯汉字的词，夹字母、数字或符号的词建议加快捷短语）。

**功能需求自动聚合**（`qq_votes.go`）：群友提的需求整理出来后，先用提 issue 时的查重（`qqDuplicate`：关键词搜开着的 issue，再由模型判断是不是同一件事）在对应仓库里找；找到了就不起草稿，在那个 issue 下维护一条计数评论「QQ 群里有 N 位群友需要这个功能」（只改这一条，靠末尾的 `<!-- qq-votes:N -->` 认出来，而且只认机器人自己发的，判断方法和汇总 issue 相同：外人留的带标记的评论直接跳过、不会被改写，否则群里会念出假的人数；分不清带标记的评论是谁发的时这次不计，不新建，免得出现两条计数），回复群友已经有人提过、在哪个 issue、现在多少人需要。同一个群友对同一个需求 30 天内只算一次（`auth_rates` 作用域 `qq-bot-vote`），改计数失败时退回这一票的名额，同一个人再说一次能记上；改计数时持跨副本锁。找不到同一个需求、或者 Windows 要先问版本时，照旧起草稿。

**把答案沉淀进文档**（`qq_faq_candidates.go`、`account/qq_faq_candidates.go`）：机器人现查 GitHub 才答上的问题（说明文档里缺这块）和答不上的问题，记成文档候选（记模型的一句话概括，提示词要求现查答上时也填，没有概括时才记原话：原话常常离开上一轮就看不懂，例如「安卓呢？可以关闭辅助码吗？」，同一件事换个说法也去不了重；问最新版本、某个版本更新了什么的问题有时效性，答上答不上都不记），存进 `qq_faq_candidates` 表（编号 F12 这样，30 天后删除；同一个问题 7 天内只记一次，`auth_rates` 作用域 `qq-bot-faq-candidate`）。只有管理员能处理：「文档缺口」列出候选和机器人当时的回答，和草稿列表一样最早的在前、表头写总数、快过期的标上还剩几天，「写进FAQ F12」（也认「写入」「加进」「加到」「放进」「记进」和「写进FAQ：F12」这类带冒号的写法，「忽略：F12」同样）用「记录答案」的流程整理成 FAQ 的一条、给知识库开 PR，没答上的在后面补上答案（「写进FAQ F12 答：……」），「忽略 F12」拿掉；「看 F12」看完整的问题和机器人当时的回答（各 600 字以内），列表里只有前 60 字。写进 FAQ 前先在数据库里认领候选（`state` 改成 `writing`），两个管理员同时发只有一个开 PR，另一个收到「正在写进 FAQ」；没开成 PR 时改回 `open`，放回不跟随已经到期的处理时间；副本中途被杀掉来不及放回时，认领超过 5 分钟就当作被打断，可以重新处理。「记录答案」开 PR 前先列出知识库仓库开着的、分支以 `qq-faq/` 开头的 PR，标题里已经有同一个问题时回「已有 PR #n」，不再新开（从文档候选写进来的照样标成已处理）；开完 PR 的回复带上写进去的答案（前 150 字）。草稿和文档候选原来只有管理员发「草稿」「文档缺口」「周报」时才看得到，没人主动去查，所以管理员 @机器人 问问题或提 issue（不是命令）时，有创建超过 1 小时还没处理的草稿或文档候选，就在第一条回复后面接一句「（还有 N 份草稿、M 条文档缺口待处理，发「草稿」「文档缺口」看）」（N、M 是各自的总数，只写有超过 1 小时的那一样），接在已有的回复里，不额外占被动回复次数；同一个管理员 12 小时一次（`auth_rates` 作用域 `qq-bot-backlog-hint`），真接上时才扣，「暂时处理不了」后面不接。

**答错了能被记下来**（`qq_bad_answers.go`、`account/qq_bad_answers.go`）：群友在追问的时间范围里回一句「不对」「答错了」「没用」这类短话（30 字以内，上一轮不是答不上），机器人认作差评。只认两种说法：去掉语气字、标点和「还是」「好像」「感觉」这类垫话后整句只剩一句否定（「不对啊」「好像还是不行」），或者以否定开头、隔一个标点或空格再接纠正内容（「不对，我是安卓」）；带「怎么」「如何」「为什么」「吗」「？」的一律不算，「删除键没用了」「候选框不对」「语音识别不准怎么办」是新问题，照常答疑（原来否定词出现在任意位置都算，新问题会被当成差评、转去把上一个问题重答一遍）。认作差评时机器人把上一轮的问答连同这句反馈存进 `qq_bad_answers` 表（不存群友身份，30 天后删除），强制现查、用强模型把上一个问题换思路重答一次，回复以「我重新查了一下：」开头；上一轮已经是重答、群友又说不对时不再现查重答（再查一遍要几十秒，答出来多半和上次差不多），把这句并进 30 分钟内同一个问题还没处理的那条差评（没有时新记一条），回复「我也没查到更好的答案，已经转给管理员看了～」，处理结果记 `retry_exhausted`；差评没记下时不说已经转给管理员，改说可以换个说法再问或在群里问问大家，记 `retry_exhausted_unrecorded`；重答失败（多半是处理时间用完）时差评照样记下，重答一栏写「（重答失败）」，记差评另限 3 秒、不跟随已经到期的处理时间。管理员发「差评」看最近两周还没处理的差评：每条编上号（B7 这样）、占两行（问题和群友的原话；重答，重答失败时是原来的回答），表头写总数；「看 B7」看完整的问答，「写进FAQ B7」用「记录答案」的流程把重答写进 FAQ（重答也不对就写「写进FAQ B7 答：……」），「忽略 B7」拿掉，处理过的（`state` 为 `done` 或 `ignored`）不再列出，周报里「群友说答错几次」照样都算。**慢的时候先回一句**：现查 GitHub、联网搜索之前回「我查一下，稍等～」，看截图之前回「我看一下截图～」；常规答疑 8 秒还没答完时先回「收到，我想一下～」；挑段落判成闲聊、要用聊天模型作答时停掉这个计时器（挑段落加聊天模型常常过了 8 秒，「在吗」曾经先收到一句「收到，我想一下～」再收到「在的」），只停计时器、不占过渡语，聊天模型答不上转去现查时照样说「我查一下」。这几句共用一个标志，一条消息最多说一句，开放平台每条消息最多被动回复 5 次。管理员发「提issue」一进来就回「收到，我先整理一下～」（带截图时是「收到，先看下截图再整理～」），之后不再回「收到，正在整理成 issue」；「记录答案」「写进FAQ」和「提文档」也是一进来先回「收到」，做完再回结果。追问能接上最近 4 轮（原来 2 轮）。每个副本第一次收到 @ 消息时记一行 `event member_role probe` 日志，确认事件里有没有带群身份；同时记一行 `event fields probe`，列出事件排好序的顶层字段名和值是数字的字段（写成「字段名=值」，不记消息内容、群和成员的标识），确认群 @ 事件里有没有带被引用的消息、字段叫什么，确认之后才能把引用的文字和图片交给模型。

**周报和自测**（`qq_digest.go`、`qq_selftest.go`、`account/qq_stats.go`）：每条消息处理完按天（北京时间）、按处理结果计数，存进 `qq_daily_stats` 表（90 天后删除）。管理员发「周报」，看最近 7 天收到多少、答上率、回复没发出去几次（群友和管理员分两行「群友 @」「管理员 @」，各算各的答上率：管理员自己试机器人的消息曾经占了一大半，混在一起看不出群友的情况；按处理结果的 `admin_` 前缀和 `limited_admin` 分，管理员发的命令、没发出去和超时的回复不带前缀，算在群友那一行）、群友说答错几次、待提交的草稿、文档缺口、这周从 QQ 提了几个 issue（按 `qq_bot.labels` 的第一个标签搜）。管理员发「自测」，机器人用生产环境真实的知识库、GitHub 和模型回答内置的 13 道题（使用问题、问人、提建议、外部软件、统计、闲聊、版本、各平台指南、没说平台的用法、含糊的报错、没说平台的「怎么卸载」），不记差评、不记文档候选、不回到群里，再让强模型按每题的要求判对错；「怎么卸载」判分前先硬性检查不超过 200 字、行数不超过平台数加 1，超了直接判不过（长度靠判分模型看不准）；回复通过数和没通过的原因，判不过的附上机器人回答的前 60 字；在时限里没答完的题单独计数（「自测 8/10 通过，1 题超时」），不算成答错，模型或 GitHub 的英文报错只写进日志，回复里只说「回答出错」；6 道一批并行，分三批，几分钟内跑完，每个管理员每天最多 3 次。改完提示词跑一次，就知道有没有把别的问题改坏。发版后也自动跑一次：副本启动、知识库预热完后按版本号认领（`auth_rates` 作用域 `qq-bot-selftest-deploy`，30 天），两个副本、同一版本重启几次都只跑一次，结果不发回群里、只写日志：汇总行 `qq bot: selftest` 带通过数、超时数、总题数、`version` 和 `trigger`（`deploy`，管理员发的是 `admin`），通过的比例低于 10/12 时打 Error 供 Loki 告警，没通过的题各记一条 Warn（问题和原因，不记回答）；副本关停打断了自测时不记汇总，免得一批超时被当成退化。原来自测只有管理员发「自测」才跑，发版后第一批群友就是实际的验证者。

**自测从修好的问题里学，每天定时跑，掉分开 issue**（`qq_selftest.go`、`account/qq_selftest_cases.go`）：管理员每把一个答案写进 FAQ（「记录答案」、「写进FAQ F编号」、「写进FAQ B编号」，都走 `recordQQAnswer`），整理后的问题和答案（和 FAQ PR 里的一样，不是群友原话）记进 `qq_selftest_cases` 表（同一个问题只留最新的答案，180 天后删除）；之后每次自测跑固定的题，加上最新的至多 8 道学到的题（和固定的题重复的跳过），判分时要求回答的要点和核对过的答案一致：曾经自测只有固定的十几道题，群里真实答错、修好过的问题改了提示词又会答错，没人发现。除了发版后跑一次，`qqImprovementJob` 每小时看一次，北京时间每天 8 点以后按日期认领（`qq-bot-selftest-daily`）再跑一次，好几天没发版时模型或上游的变化也能在一天内发现。发版后和每天定时的自测，分数都记进改进依据；固定的题通过的比例低于 10/12 时，除了汇总行打 Error，还在 `qq_bot.improvement_repo` 开一个「QQ 机器人自测掉分：版本，通过数」的 issue，列出没通过的题、原因和回答的开头（每天最多一个，`qq-bot-selftest-alert`）：原来掉分只打一条 Error 日志，要有人在 Loki 里配告警才看得到，一直没人配。学到的题不算进告警线，FAQ PR 合并前它们常常答不上。

**答不上时现查 GitHub 和网上**（`qq_research.go`）：答疑能用的查询原来都是写死的（文档段落、搜 issue、查版本、仓库统计），群里每冒出一种没想到的问题（「现在有几个开发」）就答不上。所以挑段落的模型标出 `lookup`（问开发者、贡献者、「某某是谁」、某个账号是不是开发者、某个 PR 改了什么这类要现查的问题）时回答前先查，回答的模型标成 unknown 时也查一次（回答前已经查过的不管查没查到都不再查）：给模型一个只读的 GitHub 工具 `github_get`（OpenAI 兼容的 function calling），让它自己决定查什么，例如贡献者、提交、PR、Release、issue、仓库里的文件，答疑时已经查到的用户提到的 issue 和搜到的 issue（截到 1500 字）接在问题后面一并交给它，注明不必重复调用，最多调用 5 次、整次 100 秒（同一轮里的多个调用先按顺序分剩下的名额，有名额的同时执行，结果按原来的顺序交回模型，提示词也要求互相不依赖的查询放在同一轮里一起发；原来一个一个串行跑，一轮三个查询要等三次 GitHub），剩余的处理时间减去 40 秒（留给重答）更短时按它算；离调研的截止不到 20 秒时不再给工具，用已经查到的内容作答，某一轮卡住时也停在截止前 20 秒，用查到的内容总结，不整份丢掉。查完列出和问题有关的事实（不超过 300 字），作为「现查的 GitHub 结果」交给回答的模型重答一次；模型认为不需要查、查不到或者出错时，照原来的答不上处理。常见问题在第一步就答上了，不走这一步。工具只能对 metasequoiaime 组织发 GET 请求：路径只能是 `/repos/metasequoiaime/<仓库>/…`、`/orgs/metasequoiaime…`、`/users/<账号>`（不含子路径，返回只留 login、name、type、public_repos、followers、created_at，只用来确认账号存在和注册时间，提示词要求不列所在地、公司、简介、博客、邮箱和社交账号，也不用 `web_search` 搜个人信息，回答只说他在组织仓库里的贡献；曾经整份资料交给模型，机器人把群友的所在地、公司原样发进了千人群），或者 `q` 里限定了 `repo:metasequoiaime/…` 或 `org:metasequoiaime` 的 `/search/issues`、`/search/commits`。令牌只有 contents、issues、pull_requests、metadata 的读权限，而 App 装在组织的全部仓库上、能读私有仓库，所以 `/repos` 路径发请求前先用同一个令牌 `GET /repos/{repo}`，`private` 为 false 且 `visibility` 是 public 才查，否则回复模型这个仓库不公开、不能查；检查失败同样不查（fail closed），结果按仓库在每个副本缓存一小时，失败不缓存。返回的 JSON 去掉各种链接字段和头像，人只留登录名，Release 的安装包只留文件名，正文和文件内容接口的 `content`（base64 的文件原文）都截到 400 字，单个 Release（`/releases/tags/<tag>`、`/releases/latest`）的发布说明留到 2500 字（截到 400 字时模型会对群友说「只看到了前半部分」，提示词也要求不提截断），每次结果截到 6000 字。除了 `github_get`，模型还有一个 `web_search` 工具：问到水杉以外的软件和产品、新闻、价格、技术资料等时联网搜索。EveryAPI 网关在 Anthropic Messages 的 `web_search` 服务端工具上实现了联网搜索，返回搜索结果的摘要，服务端把 `chat.url` 末尾的 `chat/completions` 换成 `messages` 去调，密钥沿用 `chat`；挑段落的 `lookup` 也覆盖这类要上网查的问题。`web_search` 只查水杉以外的东西，水杉自己的功能和隐私以文档为准，不拿网上的说法介绍水杉；网上的内容可能不准：现查列事实时只有 `web_search` 搜到的才注明「据网上资料」，GitHub 上查到的不注明，回答时网上查到的内容说「据网上查到的信息」。提到 issue 和 PR 的回复附上 GitHub 链接；开放平台只允许报备过的域名，拒收带链接的回复时去掉链接重发一次，日志记下错误码。

**向量检索**（`qq_vectors.go`、`account/qq_knowledge.go`）：目录只看得到标题和开头几十个字，答案埋在正文深处、或者群友的口语问法和文档说法对不上时挑不中；GitHub 搜索要求每个词都匹配，也搜不到意思相同、说法不同的旧 issue。所以文档段落、答疑仓库（`qq_bot.repo` 和各平台仓库）全部的 issue 和 PR 都算成向量，存在 PostgreSQL 的 `qq_knowledge_items` 表（pgvector 的 `halfvec(2048)`，几千行，精确扫描，不建近似索引）。答疑时把问题也算成向量，和模型挑段落同时进行：相似度不低于 0.45 的文档段落（最多 6 段）接在模型挑出的段落后面；相似度不低于 0.45 的 issue 和 PR（最多 5 条，写明 issue 的状态或 PR 是否合并、正文摘要和维护者的回复）列成「语义相近的历史 issue 和 PR」，回答的模型可以据此说明以前有人报过、当时怎么解决的，拿不准时要说可能相关。向量用 `chat` 端点所在服务的 `/embeddings` 接口算（把 `chat.url` 末尾的 `chat/completions` 换成 `embeddings`，密钥沿用 `chat`），模型是 `qq_bot.embedding_model`，默认 `doubao-embedding-vision`（EveryAPI 上 2048 维、单次约 0.7 秒），写 `none` 关闭。同步由一个副本做（`auth_rates` 作用域 `qq-vector-sync`，每 10 分钟一次）：文档段落按内容摘要比对，变了才重算，删掉的段落一并删除；issue 和 PR 按仓库记下同步到的更新时间（`qq_knowledge_cursors`），每次从那里往后拉，每个仓库每次最多 20 页（2000 条），首次全量回填分几次跑完；issue 另外读维护者最近的一条回复，PR 的正文已经说明改了什么，不读评论。pgvector 要由数据库超级用户在库里执行一次 `CREATE EXTENSION vector`（迁移角色装不了）：装了迁移才建这两张表，没装时这个功能关闭，答疑照旧只用目录挑段落，服务照常启动。CI 的 PostgreSQL 集成测试用 `pgvector/pgvector` 镜像。

知识库按文件名标出每篇指南的适用平台，写在段落的文档名前面（`windows` 是「Windows 正式版」，`macos`、`macos-voice` 是 macOS，`linux` 是 Linux，`android` 是 Android，`ios` 是 iOS，`migration` 是「Windows 正式版」（文档自己写着导入格式和入口以 Windows 版为准，标成通用时 Mac、安卓用户问导词库会拿到 Windows 的入口），`accessibility` 是各平台；代码里的 `qqDocPlatforms`，新增指南时要补上，「提文档」整理时可选的页面也取自这里）。FAQ 按每一问的标题打标签：「支持哪些系统，去哪里下载」「GitHub 下载太慢」「有鸿蒙」这几问标「各平台」，标题里写了平台的标对应平台，其余标「FAQ · 以 Windows 正式版为主」（faq.md 开头写着其余排查以 Windows 为主，原来整篇标成「FAQ」，曾把 Windows 才有的「候选窗补充字体」答给 Linux 用户），这些标签不进回答时告诉模型的版本清单。windows.md 的一级标题是「水杉输入法文档」，不标的话 Windows 的操作路径会被当成通用答案交给 macOS、Linux 用户。挑段落的模型按用户说的平台优先挑；用户没说平台时，问功能有没有、怎么用就每个有相关段落的平台各挑一段，描述故障就先挑 Windows 正式版和 FAQ。回答的模型对只适用于某个平台的做法要注明平台，用户明确用的是资料没有覆盖的平台时按答不上处理；回答的系统说明里列出 `qqDocPlatforms` 里的平台（去掉 FAQ、通用、各平台），告诉模型资料只覆盖部分平台时不要说其他平台没有这个功能或版本，曾经没说平台的问题只挑到 Windows 段落，回答就成了「五笔只有 Windows 有」。用户没说平台时，各平台的做法都能一行（不超过 40 字）说清就每个平台一行、只写入口，有一个平台要多步操作就只追问一句「你用的是哪个系统？」（escalate 时不追问，由草稿那一行另外追问）；资料没写到的平台或细节能用常识就说一句，不能就不提，不写「没查到」「没看到说明」。和其他输入法对比、问值不值得换时，挑水杉的功能、隐私、平台与下载段落，笼统对比不现查；水杉自己的功能和隐私只按文档说，不拿网上的说法和 issue 介绍水杉，也不把开着的 issue 数量或 0.x 版本号当成缺点。每篇一级标题下、第一个二级标题之前的导语收成「概述」一段（适用版本、系统要求常常只写在这里）；目录里每段附上正文的加粗词，没有加粗词但有两个以上「- 标签：说明」这样的列表项时列出标签（安卓「常用操作」里的换皮肤、删除、隐私原来只露出开头的「选择候选：…」），都没有时取正文开头，最多 50 字，只看标题挑不中答案只写在正文里的段落。

每条消息处理完记一行 `qq bot: handled` 日志，字段是 `outcome`、`admin` 和耗时 `ms`，不含内容和 openid；另有各阶段的耗时：`select_ms`（挑段落）、`answer_ms`（回答，含现查后的重答）、`research_ms` 和 `research_calls`（现查的总耗时和工具调用次数）、`fallbacks`（主模型失败、换备用模型的次数）、`first_reply_ms`（从开始处理到第一条不是过渡语的回复）和 `interim`（发过「收到，我想一下～」），原来只有总耗时，分不出花在哪一步，也量不出优化的效果。`outcome` 是固定的几种：`cmd_whoami`、`empty`、`screenshot_only`、`too_long`、`limited_member`、`limited_group`、`limited_admin`、`timeout`、`reply_failed`、`cmd_help`、`cmd_releases`、`cmd_search`、`cmd_issue`、`cmd_docs`、`cmd_record_answer`、`cmd_report_<结果>`、`admin_<结果>`、`answered`、`answered_escalate`、`answered_hint`、`chat`、`unknown`、`unknown_unrecorded`、`retry_exhausted`、`retry_exhausted_unrecorded`、`failed`，其中管理员反馈的结果是 `filed`、`filed_duplicate`、`existing`、`needs_info`、`needs_version`、`not_report`、`failed` 或转去答疑时的答疑结果。在 Loki 里筛出这行日志、按 `outcome` 汇总（`|= "qq bot: handled" | json` 后 `sum by (outcome)`），就能看出答上率和各类消息的占比。`reply_failed` 是回复写好了却没发出去、原因又不是链接（多半是被开放平台的内容审核拒收）：机器人会补发一句「回答没能发出来（可能被平台拦了），可以换个说法再 @我 问一次。」，命令的回复没发出去时仍记命令本身。

issue 和评论只包含模型转述的内容，不含群友的原话、昵称或 openid，并打断 @ 提及、# 引用和链接（与官网词条提交的备注相同）。消息内容不写日志，日志里只有失败原因；`qq_bot.groups` 非空时，来自其他群的消息会以 `group_openid` 记一条日志，方便查出要填的群标识。开放平台不提供群号，`groups` 只能填 `group_openid`；为空时接受机器人所在的每个群。

需要用户体系（PostgreSQL）、管理后台的 GitHub App（`admin.github`）和 `chat` 端点。`app_id` 为空时功能关闭；填写后缺任何一项服务都拒绝启动：

```json
{"qq_bot":{"app_id":"102030405","secret_env":"MSIME_QQ_BOT_SECRET","repo":"metasequoiaime/msime","labels":["source/qq"],"model":"claude-haiku-5-5","admins":["<member_openid>"]}}
```

`model` 经 `chat` 端点调用，接入地址和密钥都沿用 `chat`（生产是 EveryAPI），为空时用 `chat.model`。AI 联想的 `gpt-5.6-luna` 也能用，但整理长段描述时偶尔要 20–30 秒；EveryAPI 上的 `claude-haiku-5-5` 每次 1–4 秒，分类和去重判断都准确，输入价格更低，所以推荐单独指定它。`fallback_model` 是可选的备用模型：主模型请求失败（上游拒绝、超时）或输出不合格式时，用它重答一次，适合主模型选回答质量好但上游不够稳的模型（例如 `claude-sonnet-5-5`），再用便宜稳定的模型兜底；为空时不重试。配置了备用模型时主模型每次调用单独限时：`model`（挑段落、整理、查重、选标签）15 秒，`strong_model` 和 `chat_model` 35 秒（Sonnet 写长答案要 15 秒左右），并且不超过剩余处理时间减去 25 秒，给备用模型留出时间；剩余不到 20 秒时主模型失败就不再试备用模型。原来一律 50 秒，主模型挂起时要白等 50 秒才换备用。这是新增的配置字段，要等所有副本都运行支持它的版本之后再写进配置。每次请求都会在用户消息后追加一条含小写 json 的格式指令：EveryAPI 转发 OpenAI 模型时只在用户消息里找这个词，找不到就拒绝 `json_object` 模式。`json_object` 经 EveryAPI 转发给 Claude 时不起强制作用，`claude-sonnet-5-5` 实测约一半先写一段正文、再附 JSON 或 ```` ```json ```` 代码块，原来整段判成不合格式、换备用模型重答，多等 10–20 秒；现在要求 JSON 的每个系统说明末尾都加一句格式硬要求（整条回复只能是 JSON 对象本身，第一个字符是 `{`、最后一个是 `}`），严格解析失败时再从第一个「`{` 后面紧跟 `"`」的位置解出第一个完整对象（记 `lenient json` 日志），后面的文字不管；不从第一个 `{` 截到最后一个 `}`，正文里可能本来就有花括号。仍然解不出才算不合格式，日志只记模型和长度，不记内容。

`labels` 里的标签要先在所有可能提交到的仓库（`qq_bot.repo` 和各平台的 `repo`）里建好，GitHub App 也要在这些仓库上有 Issues 写权限。在 QQ 开放平台的「开发 → 回调配置」填写 `https://api.msime.app/v1/community/qq-bot/events` 并勾选群 @ 消息事件；保存时开放平台发来的 op 13 校验请求由服务端用同一把密钥签名应答，所以要先部署好配置再填回调地址。机器人没有认证时只有管理员能用，只能加到管理员当群主的群里；个人认证后可以设成公开，最多 500 个群。`admins`、`knowledge_repo`、`knowledge_dir` 是新增的配置字段，要等所有副本都运行支持它们的版本之后再写进配置。

## 用户皮肤社区

用户可发布自定义键盘设计、下载使用、收藏和评分（登录即可评分，不能评自己的作品），使用 Apple 登录与 PostgreSQL 共享存储，支持 K8s 多副本。接口、迁移和上线说明见 [皮肤社区](docs/skin-community.md)。

用户也可以分享候选窗皮肤包（skin.toml 加 PNG/JPEG，服务器重新编码图片），接口 `/v1/community/candidate-skins` 与限制见 [皮肤社区 · 候选窗皮肤包分享](docs/skin-community.md#候选窗皮肤包分享)。`auth.community.official_skin_publishers`（用户 ID 列表，64 位小写十六进制，默认为空，最多 50 个）指定官方发布账号，它们不受公开 20 款和每小时 10 次公开发布的限制，仍走审核和包校验。旧版本不认识这个配置项，会因未知字段拒绝启动：先让所有副本升级到新版本，再把它写进生产配置；回退版本前先删除它。

用户还可以分享客户端插件包（音效包、音乐包、指令表、特效包、辅助码表、符号集、短语表和单词本，zip 内含 plugin.toml、音频或数据文件和说明），服务器只校验和原样分发、从不执行。旧客户端不认识的后四种类型只返回给用 `kinds` 参数声明支持它们的客户端；升级时迁移会替换 `community_plugins` 的类型约束，上线顺序是先发容忍未知类型的客户端、再部署服务端、最后发布新类型的客户端。接口 `/v1/community/plugins`、包校验规则、配额与部署说明见 [插件社区](docs/plugin-community.md)。

`GET /v1/community/stats` 公开返回社区内容总量 `{skins,skin_downloads,dictionaries,replies,resource_saves,generated_at}`，供官网服务端拉取后自行缓存。下载与收藏按账号去重，注销账号的作品和互动随之移除；不含用户数和安装包上报（这两项只在管理后台概览提供）。与其他社区接口一样免令牌、按 IP 每分钟 120 次限流、响应禁用缓存，用户体系未启用时返回 503。

`GET /v1/site/download-mirrors` 公开返回官网下载页使用的镜像链接 `{lanzou_url,updated_at}`（未设置时均为空字符串），由管理员在后台「站点设置」中修改。同样免令牌，但与 `GET /v1/notices` 共用另一份按 IP 每分钟 1200 次的限流（不占用其他用户接口的 120 次），成功响应允许 60 秒公共缓存；详见 [管理后台](docs/admin.md)。

### 各平台设置同步

用户设置支持 `platform.ios.*` 字段：`nine_key`、`keyboard_layout`、`sound_enabled`、`haptics_enabled`、`haptic_strength`、`dictionary_learning`、`keyboard_skin` 和 `custom_keyboard_skin`。`keyboard_layout` 与 Android 的 `platform.android.keyboard_layout` 一样是最长 32 字节的字符串，服务端不限定取值，客户端写 `twenty_six_key`、`nine_key`、`fourteen_key` 等布局名，能表达布尔值 `nine_key` 表达不了的 14 键；`nine_key` 继续保留，旧版 iOS 客户端只读写它。自定义皮肤是最长 768 KiB 的 JSON 字符串（支持 512 KB 的照片背景）；完整设置请求上限为 1 MiB；客户端按本地皮肤模型解码并校验。公共输入方案与简繁体继续使用 `input.schema`、`input.shuangpin_schema` 和 `input.character_set`。

Android 使用 `platform.android.*` 字段：既有的主题、键盘布局、键盘高度微调（−46…55）、按键与行间距、语音快捷键、按键音、振动及强度、当前自定义键盘皮肤，以及新版设置页的 `one_handed`、`key_popup`、`swipe_down_symbols`、`space_cursor`、`space_voice`、`key_animation`、`key_sound_pack`（字段表里没有通用的 `plugins.key_sound.pack` 键，所以按键音包用这个 Android 键）、`toolbar_*` 工具栏按钮、`handwriting_*` 手写设置、`voice_language`、`voice_offline_fallback` 和整个自定义键盘皮肤库 `custom_keyboard_skins`（最长 768 KiB 的 JSON 字符串，只含设计参数，不含图片）。公共字段新增 `general.app_theme`、`helpcode.quanpin_helpcode_mode`、`helpcode.shuangpin_helpcode_mode`，以及 `input.chinese_punctuation`、`input.learning`、`input.wubi_code_hint`。无编码常用语走单独的 `/v1/users/me/phrases`（见 [用户体系](docs/user-auth.md#常用语同步)），不进设置文档。

客户端应先读取 `/v1/users/me/preferences/schema`，仅上传已支持的设置。PUT 为整份替换：必须保留其他平台的已有字段并携带读取到的 revision；遇到 409 先重新读取并让用户确认，不自动覆盖。登录凭据、网络端点和系统运行权限不进入设置同步。

## 管理后台

新增内嵌 Go 的 [Admin Web 项目](admin-web/README.md)，随同一镜像、同一端口启动，通过 `admin.msime.app` 独立 Host 提供服务。共 16 个页面：数据概览、词库 PR 审核、社区事后审核与举报、GitHub Issue 分诊、App 内用户反馈、敏感词库、用户与封禁、下载记录、公告推送（含 Telegram）、发布管理、云端监控、崩溃分组、系统状态与故障事件、服务日志（来自集群 Loki 的各副本实时日志）、角色权限与操作日志、个人中心与个人访问令牌。默认关闭，需要 PostgreSQL 12 及以上、后台迁移和 Google 管理员白名单（或独立管理员密钥）；GitHub、上游服务额度、Telegram 渠道和服务日志的 Loki 按需配置。角色权限、配置项、公开接口（`/v1/notices`、`/v1/community/reports`、遥测）以及需要客户端配合的改动见 [管理后台文档](docs/admin.md)。

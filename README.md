# 水杉云（msime-cloud）

水杉云是水杉输入法的共通网络后端，使用 Go 实现；HTTP 服务基于标准库，WebSocket 使用固定版本的 [coder/websocket](https://github.com/coder/websocket)。集中保存服务商凭据，向 Windows、macOS、iOS 和 Linux 提供 HTTP API 和实时语音 WebSocket。平台继续负责本地输入、焦点、候选合并、麦克风权限及上屏。

当前服务与各平台源码接入已实现；已通过真实客户端到合成上游的网络测试，原生宿主验收仍在进行。完整需求与未完成项见 [需求核对](docs/requirements.md)。

## 启动

需要 Go 1.25 或更高版本。

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
| POST `/v1/chat/completions` | `messages`、可选 `model`、`max_tokens`、`temperature` | Chat Completions JSON；支持联想和语音润色 |
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

语音模型由服务端固定。聊天默认保持固定模型；管理员可在 `chat.models` 配置最多 32 个可选模型 ID，`GET /v1/models` 返回默认模型和允许列表，聊天请求只能选择其中的模型。EveryAPI 凭据仅保存在服务端；聊天未指定输出长度时最多生成 2048 token，避免长语音润色被候选场景的小预算截断；现有 AI 候选客户端明确请求 512 token，请求上限为 2048。聊天仅接受非流式文本消息（system/user/assistant），不支持工具调用。支持现有 Linux/Windows 请求中的 `response_format.type=json_object` 或 `text`；客户端的 `thinking.type=disabled` 和 `enable_thinking=false` 只作兼容接收，不透传服务商专有字段。JSON 请求上限 64 KiB；语音文件上限 15 MiB，multipart 总体上限 16 MiB；上游响应上限 1 MiB。客户端必须保留取消和输入代次校验，失败时继续本地输入。

翻译上游支持 OpenAI 兼容聊天模型、DeepLX、DeepL、腾讯 TMT `TextTranslateBatch` 与小牛翻译 NiuTrans v2；客户端统一使用 DeepLX 格式，不持有供应商密钥。语音上游使用 multipart 转写协议，可解析 `text`、`transcription` 和 `result.text`。云候选上游使用 Google Input Tools 的响应格式，输出过滤控制字符、超长候选和重复项。

客户端请求不需要传 `provider` 字段。翻译 provider 由服务端配置决定；可选的 `translation_fallbacks` 按配置顺序尝试。主 provider 返回上游错误、超时或无效响应时，服务端继续下一个 provider；请求参数错误和不支持的批量请求不会切换。只有腾讯 provider 支持当前 `texts` 批量格式。

错误采用 `{"error":{"code":"...","message":"..."}}`：400 参数不合法、401 未认证、403 来源不允许、413 上传过大、415 格式不支持、429 限流、502 上游失败、503 功能关闭或并发已满、504 超时。错误不透传服务商正文。429/并发已满提供 Retry-After。额度按认证主体的 token bucket 控制：设备令牌按 `clients[].requests_per_minute`，登录用户每分钟 120 次。桶保存在每个副本的进程内存里，候选这类几乎逐键请求的热路径不写数据库，重启会重置。顶层 `replicas`（默认 1，可设 1–64）填部署的副本数，每个副本对每个主体执行 ⌈额度 / `replicas`⌉ 次/分钟（至少 1 次），突发容量同样按这个份额计；在无粘性会话的轮询转发下，所有副本合计约等于配置的额度。向上取整会让合计略高（例如 5 次/分钟、2 个副本时合计最多 6 次），而单个客户端的请求没有被均匀分到各副本时，可能在合计用满前就收到 429。`replicas` 必须随扩缩容同步修改并滚动重启，否则合计会按比例偏离配置。详见下文「多副本部署」。

浏览器访问须明确配置 HTTPS `allowed_origins`，不使用通配来源。原生客户端不需要 Origin。官网目前负责展示与下载，没有在线输入功能，不应添加不需要的云调用。

## 多副本部署

服务可以在同一个 PostgreSQL 后面运行多个副本（例如 k8s 多副本经 Cloudflare Tunnel 轮询转发、无粘性会话）。多副本部署必须启用用户体系和数据库（`auth.enabled`）：没有数据库时，本该经数据库共享的限流、AI 插画任务等状态只能退回各进程内存，副本之间互不可见，所以 `replicas` 大于 1 而未启用 `auth.enabled` 时服务拒绝启动。顶层 `replicas` 填实际副本数。

- 经 PostgreSQL 共享：账号、会话和社区数据；`auth_rates` 上的数据库限流（登录、社区接口、官网词条投稿、管理后台的 `admin`/`admin-auth`/`admin-login` 三项限额等，固定一分钟窗口）；共享译文缓存；遥测和管理后台数据；AI 插画任务存在 `skin_jobs` 表，任何副本都能轮询和取消，每主体与全局上限按整个部署计算（见[皮肤社区](docs/skin-community.md)）；官网词条投稿写 GitHub 前取跨副本的 advisory lock，同一时刻只有一个副本改滚动分支；系统状态由持有 advisory lock 的一个主副本汇总所有副本的分钟计数后统一判定（见[管理后台](docs/admin.md)「系统状态的多副本行为」）；数据库迁移在 advisory lock 下串行执行，多个副本同时启动也只会建一次表。
- 新增的配置字段 `admin.logs` 和 `request_log` 与 `replicas` 一样：先让所有副本运行新版本，再写进配置；回退前先删除。新版本启动时会替换 `admin_role_permissions` 的权限约束（加入 `view_logs`），旧版本副本不受影响；按最小权限部署时这一步也要先用有 DDL 权限的账号执行 `-migrate-users`。
- 滚动升级时新旧版本短暂并存：旧版本副本仍在本进程内保存 AI 插画任务（新旧副本互相看不到对方的任务，客户端可能收到 404 后重新生成）、不取词条投稿锁、按旧逻辑各自判定系统状态（当天可用分钟数可能多计）。所有副本升级完成后恢复。旧版本不认识顶层 `replicas`，而配置中的未知字段会让服务拒绝启动：先让所有副本都运行新版本，再在配置里加入 `replicas` 并滚动重启；回退到旧版本（如 `kubectl rollout undo`，它不会回退单独管理的 ConfigMap）之前，先从配置中删除 `replicas`，否则重启的旧版本副本会反复启动失败。按最小权限部署时，先用有 DDL 权限的账号执行 `-migrate-users`，再给运行角色授予新表 `skin_jobs`、`admin_service_minutes`、`admin_service_verdicts` 的 `SELECT, INSERT, UPDATE, DELETE`。
- 顶层 `client_ip_header` 同理：旧版本不认识它。上线顺序是先让所有副本运行新版本，再在配置里加入 `"client_ip_header": "CF-Connecting-IP"` 并滚动重启；回退到旧版本之前先删除它。过渡期也可以继续只写旧的 `word_submissions.client_ip_header`，新旧版本都接受，新版本会把它用于所有按地址的限额。生产经 Cloudflare Tunnel 和 traefik 转发，TCP 对端始终是 traefik，不设置这一项时匿名遥测、匿名开户和账号接口的按地址限额由全体用户共用一份。
- 顶层 `site_proxy_secret_env`（官网 BFF 的共享密钥所在的环境变量名，见 [用户体系](docs/user-auth.md)）同理：旧版本不认识它。上线顺序是先让所有副本运行新版本，再把密钥写进部署的 Secret（与其他密钥一样以环境变量注入，例如 `MSIME_SITE_PROXY_SECRET`，值用 `openssl rand -hex 32` 生成，并在官网 Pages 设为同值的 Secret `SITE_PROXY_SECRET`），然后在配置里加入 `"site_proxy_secret_env": "MSIME_SITE_PROXY_SECRET"` 并滚动重启；配置了变量名但读不到 32–256 个可见 ASCII 字符时服务拒绝启动。回退到旧版本之前先删除这一项。轮换密钥时先改后端 Secret 并滚动重启、再改官网，后端不支持新旧两个密钥并存，所以在两边改完之前，官网的所有访客共用 Cloudflare 出口地址的一份按地址额度（账户接口每分钟 120 次），流量大时会出现 429；轮换宜选在低峰期，并尽快改完官网。
- 社区收藏和刷新令牌宽限期（`community_skin_saves`、`community_candidate_skin_saves`、`community_plugin_saves` 三张新表，`auth_used_refresh` 新增 `used_at` 列）：运行角色有 DDL 权限时启动会自动迁移；按最小权限部署时先用有 DDL 权限的账号执行 `-migrate-users`，再给运行角色授予三张新表的 `SELECT, INSERT, UPDATE, DELETE`。旧版本副本不读新表，插入 `auth_used_refresh` 时由 `used_at` 的默认值补上时间，滚动升级期间可以并存；落到旧副本的 30 秒内重放仍按旧规则撤销会话。
- 按副本计算：主接口 token bucket（设备令牌和登录用户），每个副本执行 ⌈额度 / `replicas`⌉；`max_concurrent` 并发槽（实时语音会话也各占所在副本的一个）；插件发布 4 个、下载 8 个名额，原生 Engine 查询 4 路，候选皮肤图片解码 2 路，词库快照恢复 1 路；管理后台服务日志的日志流每个管理员 2 个、合计 10 个（同一个管理员的两个连接落在不同副本时可以各开 2 个）。这些上限的总量随副本数增加，设置时按单个副本的资源计算。
- 可容忍的短暂不一致：敏感词改动最多 30 秒后在所有副本生效；管理后台的全局搜索索引在各副本内存里分别建立，可能短时不同；一个副本写 GitHub 后，其他副本的 GitHub 读缓存最多 60 秒后才看到变化；上游调用指标先在内存中累积、每分钟写回数据库，副本被强制终止时最多丢失最近约 1 分钟的计数。

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

服务默认调用 `https://api.niutrans.com/v2/text/translate`，也可在 `url` 中指定同样的 HTTPS 接口。App ID 和 API Key 只通过服务进程环境注入；服务端按 NiuTrans v2 协议生成 `authStr`，不会把供应商凭据转发给客户端。小牛翻译接口是单条请求，批量 `texts` 仍只支持腾讯 TMT。

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
用户配置目录下 `msime-cloud/credentials.json`（权限 0600，可用 `MSIME_CLOUD_CONFIG_DIR` 指定目录），刷新令牌经文件锁串行使用，避免多个命令同时刷新时重放旧令牌导致会话被撤销。

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

## 用户皮肤社区

用户可发布自定义键盘设计、下载使用、收藏和评分（登录即可评分，不能评自己的作品），使用 Apple 登录与 PostgreSQL 共享存储，支持 K8s 多副本。接口、迁移和上线说明见 [皮肤社区](docs/skin-community.md)。

用户也可以分享候选窗皮肤包（skin.toml 加 PNG/JPEG，服务器重新编码图片），接口 `/v1/community/candidate-skins` 与限制见 [皮肤社区 · 候选窗皮肤包分享](docs/skin-community.md#候选窗皮肤包分享)。`auth.community.official_skin_publishers`（用户 ID 列表，64 位小写十六进制，默认为空，最多 50 个）指定官方发布账号，它们不受公开 20 款和每小时 10 次公开发布的限制，仍走审核和包校验。旧版本不认识这个配置项，会因未知字段拒绝启动：先让所有副本升级到新版本，再把它写进生产配置；回退版本前先删除它。

用户还可以分享客户端插件包（音效包、音乐包、指令表、特效包、辅助码表、符号集、短语表和单词本，zip 内含 plugin.toml、音频或数据文件和说明），服务器只校验和原样分发、从不执行。旧客户端不认识的后四种类型只返回给用 `kinds` 参数声明支持它们的客户端；升级时迁移会替换 `community_plugins` 的类型约束，上线顺序是先发容忍未知类型的客户端、再部署服务端、最后发布新类型的客户端。接口 `/v1/community/plugins`、包校验规则、配额与部署说明见 [插件社区](docs/plugin-community.md)。

`GET /v1/community/stats` 公开返回社区内容总量 `{skins,skin_downloads,dictionaries,replies,resource_saves,generated_at}`，供官网服务端拉取后自行缓存。下载与收藏按账号去重，注销账号的作品和互动随之移除；不含用户数和安装包上报（这两项只在管理后台概览提供）。与其他社区接口一样免令牌、按 IP 每分钟 120 次限流、响应禁用缓存，用户体系未启用时返回 503。

`GET /v1/site/download-mirrors` 公开返回官网下载页使用的镜像链接 `{lanzou_url,updated_at}`（未设置时均为空字符串），由管理员在后台「站点设置」中修改。同样免令牌，但与 `GET /v1/notices` 共用另一份按 IP 每分钟 1200 次的限流（不占用其他用户接口的 120 次），成功响应允许 60 秒公共缓存；详见 [管理后台](docs/admin.md)。

### 各平台设置同步

用户设置支持 `platform.ios.*` 字段：`nine_key`、`sound_enabled`、`haptics_enabled`、`haptic_strength`、`dictionary_learning`、`keyboard_skin` 和 `custom_keyboard_skin`。自定义皮肤是最长 768 KiB 的 JSON 字符串（支持 512 KB 的照片背景）；完整设置请求上限为 1 MiB；客户端按本地皮肤模型解码并校验。公共输入方案与简繁体继续使用 `input.schema`、`input.shuangpin_schema` 和 `input.character_set`。

客户端应先读取 `/v1/users/me/preferences/schema`，仅上传已支持的设置。PUT 为整份替换：必须保留其他平台的已有字段并携带读取到的 revision；遇到 409 先重新读取并让用户确认，不自动覆盖。登录凭据、网络端点和系统运行权限不进入设置同步。

## 管理后台

新增内嵌 Go 的 [Admin Web 项目](admin-web/README.md)，随同一镜像、同一端口启动，通过 `admin.msime.app` 独立 Host 提供服务。共 15 个页面：数据概览、词库 PR 审核、社区事后审核与举报、GitHub Issue 分诊、敏感词库、用户与封禁、下载记录、公告推送（含 Telegram）、发布管理、云端监控、崩溃分组、系统状态与故障事件、服务日志（来自集群 Loki 的各副本实时日志）、角色权限与操作日志、个人中心与个人访问令牌。默认关闭，需要 PostgreSQL 12 及以上、后台迁移和 Google 管理员白名单（或独立管理员密钥）；GitHub、上游服务额度、Telegram 渠道和服务日志的 Loki 按需配置。角色权限、配置项、公开接口（`/v1/notices`、`/v1/community/reports`、遥测）以及需要客户端配合的改动见 [管理后台文档](docs/admin.md)。

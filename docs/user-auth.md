# 用户体系

用户体系使用 PostgreSQL，支持 Apple、Google、微信网站扫码登录，以及阿里云短信、Lark SMTP 邮箱验证码登录。未配置的提供方保持关闭，`GET /v1/auth/providers` 返回实际状态。已有设备令牌可继续调用在线输入接口，但不能访问用户资料。

## 配置与迁移

参考 `config.example.json` 的 `auth` 配置，设置 `enabled: true`，通过环境变量提供数据库 URL 和至少 32 字节随机 `MSIME_AUTH_PEPPER`。密钥应存于 Vault，不提交到源码。

数据库的 `server_encoding` 和连接的 `client_encoding` 都必须为 `UTF8`；服务及 `-migrate-users` 在建立连接时检查，不兼容时立即报「用户数据库及客户端连接编码必须为 UTF8」。`SQL_ASCII` 无法正确处理中文字符长度及部分 JSON 操作，其他数据库编码也不能完整保存本服务支持的 Unicode 文本。升级前用服务相同的连接配置执行 `SHOW server_encoding; SHOW client_encoding;`，确认两项均为 `UTF8`，不要在数据库 URL、角色或数据库默认参数中把 `client_encoding` 改为其他编码。新建数据库时显式指定编码，例如：

```sh
createdb --encoding=UTF8 --template=template0 --lc-collate=C --lc-ctype=C msime
```

已有数据库不能通过 `SET client_encoding` 改变其存储编码。不兼容的部署需先备份并检查现有文本数据，再建立 UTF-8 数据库、恢复并验证数据，最后切换服务连接；完成之前新版本会拒绝启动。启动检查只读取连接握手参数，不修改数据库或数据，也不要求运行账号拥有额外权限。

服务启动时会检查表是否齐全，缺表就自己补迁移，所以运行账号有 DDL 权限时不需要任何手工步骤，新增表的版本直接滚动更新即可。

生产上如果按下面的最小权限方案部署，运行账号没有 DDL 权限，自动迁移会失败并在启动日志里要求手工迁移。这种部署仍然按原来的方式做，用有 DDL 权限的账号先执行：

```sh
msime-server -config /config/config.json -migrate-users
```

迁移使用事务和 PostgreSQL advisory lock，可重复运行，多副本同时启动也会串行执行、后到的跑成空操作。生产可由运维迁移，再给运行账号授予本数据库的 CONNECT、public schema USAGE，以及迁移建出的所有表的 SELECT/INSERT/UPDATE/DELETE 和所有序列的 USAGE/SELECT；运行账号不需要超级用户、建库或建角色权限。需要授权的不只是 `auth_*` 表：即使没有启用管理后台，启动检查、社区接口、遥测上报和公告接口也会读写用户数据、社区、`admin_*`（如 `admin_events`、`admin_crash_groups`、`admin_notices`、`admin_sensitive_words`、`admin_sensitive_hits`）、`community_reports`、`word_submissions`、`site_settings` 和 AI 插画抽卡任务的 `skin_jobs` 等表，缺任何一张的权限，服务都会在启动时报「迁移后仍缺少必需的表」。最简单的做法是在迁移后整体授权，并配置 default privileges 让以后新建的表自动授权：

```sql
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO msime_backend;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO msime_backend;
ALTER DEFAULT PRIVILEGES FOR ROLE msime_migrator IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO msime_backend;
ALTER DEFAULT PRIVILEGES FOR ROLE msime_migrator IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO msime_backend;
```

其中 `msime_backend` 是运行账号，`msime_migrator` 是执行迁移的账号，按实际角色名替换。管理后台各表的说明见 [admin.md](admin.md) 的部署步骤。连接生产 PostgreSQL 应启用 TLS；使用私有 CA 时挂载 CA 并设置 `sslmode=verify-full&sslrootcert=...`。

数据库保存用户、身份、验证码摘要、会话摘要和限流计数；不保存明文验证码或会话令牌，不按同名邮箱自动合并第三方身份。服务每小时清理过期挑战、会话和限流计数。数据库需要纳入备份；本服务不提供数据备份功能。

## 提供方

- Apple / Google：`client_ids` 配置本应用注册的 Client ID。客户端先请求挑战，将返回的 `nonce` 原样传入官方登录 SDK，再把 ID Token 提交给后端。后端校验签名、发行方、受众、有效期和 nonce。不得使用其他应用的 Client ID。官网登录（Google Identity Services，经官网的 Cloudflare Pages Functions 代理调用同一套 `/v1/auth` 接口）使用的 Google「Web 应用」Client ID 也必须列在 `auth.google.client_ids` 里，否则官网提交的 ID Token 因受众不符被拒绝。
- Google 桌面端（macOS/Windows/Linux）：在 `google.desktop` 配置 Google Cloud 中类型为「桌面应用」的 `client_id` 和密钥环境变量名 `secret_env`（示例配置为 `MSIME_GOOGLE_DESKTOP_CLIENT_SECRET`），该 `client_id` 必须同时列在 `client_ids` 中；同时通过 `token_key_env`（示例配置为 `MSIME_PROVIDER_TOKEN_KEY`）指定的环境变量提供标准 base64 编码的 32 字节密钥。客户端在本机监听回环端口，创建挑战时把 `http://127.0.0.1:<端口>/callback`（或 `[::1]`）作为 `target`；服务端生成 nonce、state 和 PKCE verifier，返回 `authorization_url`。客户端在浏览器打开该地址，回调时校验 `state` 与 URL 中的一致，再把 `code` 作为 credential 提交。服务端用客户端密钥和 PKCE verifier 换码并校验 ID Token，同一事务内登录、更新 Google 资料（邮箱、昵称、头像）并以 AES-256-GCM 加密保存 refresh token（附加数据为 `provider:subject`）；Google 未返回 refresh token 时保留已保存的那个。客户端不持有密钥。不传 `target` 时仍是 ID Token 流程。
- Google 桌面端上线顺序：`GET /v1/auth/providers` 的 `google` 只反映 `client_ids` 是否非空，不区分桌面端是否可用；只要 `client_ids` 已配置（生产已有网页/Android 的 Client ID），桌面客户端就会显示 Google 登录按钮，而未配置 `google.desktop` 时带 `target` 的挑战返回 503 `provider_disabled`。旧版本服务不读取挑战上的 `redirect_uri` 和 PKCE verifier，滚动发布期间若登录请求落到旧副本，会把授权码当作 ID Token 校验并返回 401。因此按以下顺序上线：先发布新版本服务（不配置 `google.desktop`），确认所有副本都已更新；再写入密钥与 `token_key_env` 对应的环境变量并配置 `google.desktop`，重新发布；最后才发布带 Google 登录的桌面客户端。回退服务版本前先移除 `google.desktop`。
- 微信：配置本应用 `app_id`、密钥环境变量及已登记的 HTTPS `redirect_uri`。这是网站扫码登录，不是小程序或移动应用 SDK 登录。前端打开 `authorization_url`，在回调验证 `state == challenge_id`，再提交 code。回调页面由客户端项目提供。
- 阿里云短信：配置 region、AccessKey 环境变量、审核通过的签名及短信模板。模板参数为 `code`，六位数字、五分钟有效。手机号使用 `+8613800138000` 这样的 E.164 格式。国际短信还需相应发送资质与模板。
- Lark 邮箱：配置实际 SMTP 主机、邮箱账号、发件地址和应用密码。支持 465 隐式 TLS 或 587 STARTTLS，强制证书验证。示例主机需按邮箱所在区域确认；不会自动发送测试邮件。
- 头像：用户对象带 `avatar_url`，上传的自定义头像优先，其次是 Google 头像（只给出 `googleusercontent.com` 的 https 地址），都没有时省略、客户端显示昵称首字；`email` 是已绑定 Google 身份的已验证邮箱，只出现在登录、刷新和 `/v1/users/me` 的响应里。Google 登录时，若用户昵称为空或仍是生成的「水杉小鹿·XXXXXX」，用 Google 昵称替换；用户自己改过的昵称不会被覆盖。
- 自定义头像存储：`avatars` 配置 Cloudflare R2 的 `bucket`、绑定自定义域名后的公开地址 `public_base_url`（须为 https 域名根地址，生产为 `https://media.msime.app`），以及账号 ID、Access Key ID、Secret Access Key 三个环境变量名（示例配置为 `MSIME_R2_ACCOUNT_ID`、`MSIME_R2_ACCESS_KEY_ID`、`MSIME_R2_SECRET_ACCESS_KEY`）。`bucket` 为空时上传关闭，`PUT /v1/users/me/avatar` 返回 503 `avatar_upload_disabled`。配置了 `bucket` 但三个环境变量有任何一个没有值时（例如密钥还没同步进 Secret），服务照常启动，只关闭上传并在启动日志中记录一条 ERROR（`avatar uploads disabled`，带缺失的变量名），不会因为这个可选功能整体起不来；`public_base_url` 格式不对或变量名为空则仍是配置错误，启动失败。服务端只写入和删除对象：上传的 PNG/JPEG（最多 1 MiB、边长不超过 4096、JPEG 最多 32 个扫描段）裁成居中正方形并重新编码为 256×256 JPEG，存为 `avatars/<随机值>.jpg`，`Cache-Control: public, max-age=31536000, immutable`；每次上传换新键，旧对象随即删除，注销账号时一并删除。头像登记和删除在用户行锁内重新检查封禁；封禁先提交时返回 403 `account_banned`，保留原头像登记和旧对象，已上传但未登记的新对象会清理。Access Key 只需该 bucket 的对象读写权限。

## 登录与账号管理

1. `POST /v1/auth/challenges`：`{"provider":"email","target":"user@example.com","purpose":"login"}`。返回 `challenge_id`，不会返回验证码。第三方登录省略 target。同一邮箱已有未过期验证码时，服务端为本次请求建立新的挑战但复用原验证码，不重复发送邮件；响应额外带 `reused:true`，首次发送为 `false` 或省略。验证码仍只在各自挑战上可用，5 分钟后失效。
2. `POST /v1/auth/login`：`{"challenge_id":"...","credential":"..."}`。credential 为验证码、ID Token 或微信 code。首次有效登录自动创建账号，返回 access_token、refresh_token 和 user。
3. 使用 `Authorization: Bearer <access_token>` 调用在线输入接口及 `GET /v1/users/me`。访问令牌有效期 15 分钟，会话最长 30 天。
4. `POST /v1/auth/refresh`：提交 refresh_token，原访问令牌和刷新令牌立即失效。客户端应串行刷新。旧刷新令牌在被轮换后 30 秒内再次提交时，视为并发刷新输掉了竞争（官网 BFF、多个浏览器标签页同时刷新），返回 409 `refresh_superseded`，会话不撤销，调用方应改用并发请求已经拿到的新令牌，而不是重新登录；超过 30 秒的重放仍会撤销该会话，返回 401，需重新登录。会话已退出或已撤销时，30 秒内的旧令牌同样返回 401。令牌应存于操作系统安全存储，不放入 URL。轮换时间记在 `auth_used_refresh.used_at`，加列之前已轮换的令牌按超过 30 秒处理。
5. `POST /v1/auth/logout`：`{"all":false}` 退出当前会话，true 退出此用户所有会话。
6. `PATCH /v1/users/me`：`{"display_name":"昵称"}`，去除首尾空白后最长 64 字符，不能含 NUL，否则返回 400 `invalid_display_name`；空昵称重置为默认昵称。在用户行锁内重新检查封禁；封禁先提交时返回 403 `account_banned`，原昵称保持不变，包括重置昵称的请求。
7. 绑定其他身份：创建挑战时使用 `purpose: link`，创建和验证均携带同一用户的访问令牌。绑定与 `DELETE /v1/users/me` 注销操作均要求最近 10 分钟内重新登录。注销删除用户、身份、会话及关联挑战。

## 我的设备

- `GET /v1/users/me/sessions` 返回本账号未撤销、未过期的会话，最多 50 条，按最近活跃倒序：`{"sessions":[{"id","platform","name","app_version","created_at","last_active","current"}]}`。`platform`、`name`（设备型号）、`app_version` 从登录时记录的 User-Agent 解析：认 `msime-<平台>/<版本> (<型号>; <系统>; edition=<版本 id>)`（例如 Android 真实账号登录时发送的 `msime-android/2.1.0 (Pixel 8; Android 15; edition=pinyin)`）、旧客户端的 `MSIME/Android` 这类写法和常见浏览器；解析不出的字段为空字符串。User-Agent 只在登录时记录，刷新令牌不更新。`last_active` 取 `greatest(created_at, 访问令牌到期时间 - 15 分钟)`，即最近一次签发访问令牌的时间。`current` 标出发起请求的会话。
- `DELETE /v1/users/me/sessions/{id}` 撤销自己的一个会话，204；会话不存在或属于其他用户时一律 404 `session_not_found`，不区分两种情况。撤销当前会话等同于退出登录。

## 常用语同步

`GET /v1/users/me/phrases` 返回 `{"revision","phrases":[{"id","text","group","position"}]}`，从未上传过时 revision 为 0、列表为空。`PUT` 整份替换，请求体 `{"revision","phrases":[…]}`，revision 必须等于当前值，否则 409 `revision_conflict`（客户端先重新读取再合并）。限制：最多 500 条，`text` 1–2000 个 UTF-16 码元、不能含 NUL，`group` 最多 32 个字符，`id` 1–64 字节且不重复，整个请求体最多 256 KiB。存在 `user_phrases` 表（每用户一行，与 `user_preferences` 同形），注销账号时删除。

## App 内反馈

`POST /v1/feedback` 需要用户会话，设备的匿名账号也可以提交。请求是 multipart：`payload` 部分是 JSON `{"type":"bug|suggestion|dictionary","text","platform","app_version","edition","diagnostics"?}`，`text` 1–500 个字符；`diagnostics` 可选，只允许 `device`、`os`、`app_version`、`edition`、`scheme`、`keyboard_layout`、`skin`、`ime_enabled`、`ime_default` 这些键，值是不超过 256 字节的字符串，出现其他键或值类型不对时整份 400。可附最多 3 个 `screenshots` 部分（PNG 或 JPEG，单张不超过 1 MiB、JPEG 最多 32 个扫描段），服务端解码后重新编码，丢掉 EXIF 等元数据，再存进 `feedback_screenshots` 表，不进头像存储、不提供公开地址。成功返回 201 `{"id","status":"received"}`。限流：每个用户每小时 5 次、每天 20 次，每个客户端地址每天 50 次，计数在 `auth_rates`，所有副本共享，超出返回 429 和 `Retry-After`。反馈保存 180 天，由 `Store.Prune` 清理，注销账号时级联删除；管理后台的「用户反馈」页查看和标记处理（见 [管理后台](admin.md)）。

## Apple 网页登录（安卓）

安卓没有 Apple 官方 SDK，走网页授权加 PKCE 式的一次性授权码，回调地址里既没有挑战 ID 也没有 ID Token。

1. 客户端生成 32 字节随机 `code_verifier`（base64url 无填充，43 字符），`POST /v1/auth/apple/web` 提交 `{"code_challenge":"<base64url(SHA-256(verifier))>","code_challenge_method":"S256","app":"<applicationId>","purpose":"login"|"link"}`。`app` 只认六个安卓版本的 applicationId（`app.msime.android` 与 `.pinyin`、`.wubi`、`.japanese`、`.vietnamese`、`.tibetan`，与 msime 仓库 `shared/contracts/editions.json` 一致），它也是回调跳回 App 的 URL scheme。`purpose=link` 需要最近 10 分钟内登录的会话。返回 201 `{"authorization_url","expires_in":300}`：`client_id` 是 Services ID `app.msime.signin`，`redirect_uri` 是 `https://api.msime.app/v1/auth/apple/callback`，`response_mode=form_post`，`scope=name email`，`state` 是挑战 ID，`nonce` 由服务端生成。
2. Apple 以表单 POST 回调 `/v1/auth/apple/callback`。这是全站唯一不检查 Origin 的路由，也不返回 CORS 头。服务端校验 state 对应的未过期挑战、ID Token（受众在 `auth.apple.client_ids` 里、nonce 等于挑战 nonce），把校验后的身份（subject、邮箱、是否验证、首次授权时的名字）写到挑战行上，签发 32 字节随机授权码，库里只存它的 SHA-256。响应是 200 的自包含落地页：CSP 为 `default-src 'none'; style-src 'unsafe-inline'`，另用哈希只放行一行跳转脚本；按钮链接 `<app>://auth/apple?grant=<grant>`，脚本尝试 `location.replace` 到同一地址（Custom Tab 可能拦截没有用户手势的外部 scheme，按钮是后备）。失败时链接 `<app>://auth/apple?error=<code>`，`code` 为 `cancelled`、`apple_error`、`invalid_token` 或 `unavailable`；state 无效时不知道该回哪个 App，只显示文字。页面不回显任何请求内容。
3. 客户端用授权码和 verifier 调 `POST /v1/auth/apple/web/login {"grant","code_verifier"}`，响应与 `/v1/auth/login` 相同，会话记录这次请求的 User-Agent。授权码只能兑换一次（校验失败也作废），签发后 120 秒内有效，verifier 的 S256 必须与发起时的 challenge 相等（常量时间比较）；绑定流程要求请求带最近登录、且与发起时同一用户的会话，否则 400 `invalid_grant`。
4. `POST /v1/auth/challenges` 拒绝 `provider=apple_web`，`POST /v1/auth/login` 拒绝 apple_web 的挑战（400 `unsupported_challenge`），截获的 ID Token 不能走老路。`GET /v1/auth/providers` 的 `apple_web` 只有 Services ID 已加进 `auth.apple.client_ids` 时为 true。上线顺序见 README「多副本部署」。

伪造回调（login CSRF）拿不到对应的 verifier，兑换会失败；截获授权码的一方同样缺 verifier。

## 云端数据

- `GET /v1/users/me/data` 返回 `{"bytes","sections":[{"id","bytes","items"}]}`，分区依次为 `preferences`、`dictionary`、`phrases`、`clipboard`、`community`、`voice`、`avatar`。大小由一条 SQL 用 `pg_column_size` / `octet_length` 汇总，是统计信息而不是磁盘占用；头像在对象存储里，只报告有没有。
- `GET /v1/users/me/data/export` 流式返回 `msime-data-<日期>.zip`：`profile.json`（ID、昵称、创建时间、只含 provider 名的身份列表）、`preferences.json`、`dictionary.ndjson`（与词库快照同一格式）、`phrases.json`、`clipboard.json`（云剪贴板的密文和登记设备）、`community.json`（作品元数据）、`sessions.json`（设备信息与时间，不含令牌摘要）。每个用户每天 3 次（`auth_rates` 键 `export:<uid 摘要>`），超出 429 带 `Retry-After`；路由时限 2 分钟。除词库外的 6 个 JSON 文件先一次批量读取，全部成功后才开始发送 ZIP，词库仍流式写入。开始传输后出错只能中断连接，客户端拿到的是不完整的 zip。导出内容不写日志。
- `DELETE /v1/users/me/data {"sections":[…]}` 要求最近登录，分区取 `preferences`、`dictionary`、`phrases`、`clipboard`、`voice`，至少一个、不能重复，返回 204。一个事务内完成：偏好和常用语清空并推进 `revision`，从未同步过的分区也会创建 `revision=1` 的空文档，重复清空继续推进版本号；其他设备拿旧 `revision`（包括首次同步前的 `0`）写入会 409 并拉到空文档；词库删除条目、覆盖层、位置、选择次数和包含词条原文的历史变更，只保留本次新写入的 `reset` 变更并推进 `revision`，旧游标会直接读到它，开着同步的设备会整份重载，而不是把旧数据推回来；剪贴板删除条目、当前密钥、封装和全部设备登记（设备名、公钥、登记时间），保留同步开关和保留天数，开着同步的设备之后重新登记，由第一台设备重新生成密钥。账号、会话和社区作品不在其中，分别走注销账号、我的设备和作品管理。

## 发送下载链接

`POST /v1/users/me/download-link {"platform":"windows|macos|linux|harmony-pc|ios|android|harmony"}` 把固定模板的邮件发到账号已验证的邮箱：邮箱登录身份优先，其次是 `email_verified` 的 Google 邮箱。不接受收件地址，正文里只有 `https://msime.app/download/?release=<platform>`，不放任何用户输入。返回 202 `{"sent_to":"u***@example.com"}`；没有已验证邮箱返回 409 `no_verified_email`（客户端改用复制链接），邮件未配置返回 503 `provider_disabled`，发送失败返回 503 `delivery_failed`（不透传 SMTP 错误）。每个用户每小时 3 次、每天 10 次，同时计入验证码共用的全站每天 450 封。邮件与验证码共用同一套 SMTP 连接、认证和发送代码。SMTP 在 `RCPT` 阶段明确永久拒收的地址会进入 30 天抑制表，后续请求直接返回 `delivery_failed`，并退回本次全站额度；邮件已被 `DATA` 接受后才产生的异步退信无法由 SMTP 同步接口判断，仍需依赖供应商的退信反馈处理。收件人是 Apple 隐藏邮箱时，需要先在 Apple 后台登记发件域名的 Private Email Relay。

## 云剪贴板保留与置顶

`GET /v1/users/me/clipboard` 的响应带 `retention_days`（0 表示一直保留），条目带 `pinned` 和 `device`（写入时所用会话的设备名，从登录时的 User-Agent 解析，认不出为空字符串）。置顶的条目排在最前，不受保留期清理，也不会被 50 条上限挤掉。`PUT /v1/users/me/clipboard/retention {"days":0|1|7|30}` 设置保留天数，每小时的清理任务删除超期的未置顶条目；它不改同步开关，`PUT /clipboard/settings` 的请求体保持不变。`PUT /v1/users/me/clipboard/{id}/pin {"pinned":bool}` 置顶或取消，条目不存在或属于别人时 404。

## 云剪贴板端到端加密

云剪贴板的文字只在设备上加密和解密，服务端只保存密文。协议（编码、密钥派生、封装格式、客户端驱动）是 msime 仓库的 `docs/cloud-clipboard-e2e.md`，服务端、Rust 驱动和各平台客户端共同遵守，这里只写服务端的行为。服务端验证不了密码学内容，只检查格式：`device_id` 必须等于 `hex(SHA-256(公钥)[0..16])`，公钥是 43 个字符的 base64url（32 字节），封装是 `"v1."` 加 139 个 base64url 字符（104 字节），条目密文是 `"v1."` 加 base64url、总长不超过 16384 个字符、解码后至少 41 字节，`key_id` 是 32 个、`dedup` 是 64 个小写十六进制字符。base64url 一律不带填充，按规范编码严格解码（换行、填充和末尾多余的位都拒绝）。

- `GET /v1/users/me/clipboard/keys?device_id=<id>` 返回 `{"enabled","key_id","wrap","devices":[{"device_id","public_key","name","created_at","approved"}]}`。没有密钥时 `key_id` 为 null，`wrap` 是当前密钥给这台设备的封装、没有时为 null，`approved` 表示设备持有当前密钥的封装，设备按登记时间排序。`device_id` 缺失或格式不对返回 400 `invalid_device_id`。顺带记下该设备最近一次来取的时间（`last_seen`，不对外返回）。
- `PUT /v1/users/me/clipboard/devices/{device_id} {"public_key","name"}` 登记设备，已登记时只改名；`name` 不超过 64 个字符、不含控制字符，可以为空。格式不对返回 400 `invalid_clipboard_device`。每个账号最多 20 台，满了先淘汰最早登记、尚未批准的一台，都已批准时返回 409 `clipboard_device_limit`。云剪贴板关闭时也能登记。
- `DELETE /v1/users/me/clipboard/devices/{device_id}` 在用户行锁内检查批准状态，删除待批准设备和它的旧封装，不存在返回 404 `not_found`。已持有当前密钥封装的设备返回 409 `clipboard_key_rotation_required`，设备、封装和条目保持不变；必须通过换钥请求的 `revoke` 移除，迟到的拒绝请求不能删除刚被批准的设备。
- `POST /v1/users/me/clipboard/keys {"previous_key_id","key_id","wraps":[{"device_id","wrap"}],"revoke":[]}` 换钥。在持有用户行锁的事务里比较 `previous_key_id` 与当前密钥编号（没有密钥时为 null），不等返回 409 `clipboard_key_conflict`，新 `key_id` 必须与当前编号不同，否则返回 400 `invalid_clipboard_key`，不会删除条目、封装或设备；比较后写入保证并发换钥只有一个成功。然后删除 `revoke` 里的设备、全部条目和全部旧封装，写入新密钥编号和封装。`wraps` 1 到 20 项、设备不重复、都已登记且不在 `revoke` 里，`revoke` 最多 20 项，否则 400 `invalid_clipboard_key`。云剪贴板关闭时返回 403 `clipboard_sync_disabled`。
- `PUT /v1/users/me/clipboard/keys/{key_id}/wraps/{device_id} {"wrap"}` 批准一台设备，已有封装时替换。封装格式不对 400 `invalid_clipboard_key`，`key_id` 不是当前密钥 409 `clipboard_key_conflict`，设备没登记 404 `not_found`。
- `GET /v1/users/me/clipboard` 返回 `{"enabled","retention_days","key_id","items":[{"id","ciphertext","key_id","updated_at","pinned","device"}]}`，只含当前密钥加密的条目。查询参数 `q` 被忽略，搜索由客户端解密后进行。
- `POST /v1/users/me/clipboard {"ciphertext","key_id","dedup"}` 返回一条条目。带非空 `text` 返回 400 `clipboard_e2e_required`（旧版本客户端），密文或 `dedup` 格式不对 400 `invalid_clipboard_ciphertext`，云剪贴板关闭 403 `clipboard_sync_disabled`，`key_id` 不是当前密钥（包括还没有密钥）409 `clipboard_key_conflict`。`dedup` 相同的条目只保留一条：保留 ID 和置顶状态，换成新密文并移到最前。已有 50 条且全部置顶时，上传新 `dedup` 返回 409 `clipboard_limit`，原条目保持不变；更新已有 `dedup` 仍可成功，取消一条置顶或删除一条后可继续上传新内容。
- `PUT /v1/users/me/clipboard/settings {"enabled":false}` 除了条目，还删除当前密钥、封装和全部设备登记；再次打开后由第一台设备重新生成密钥。`DELETE /v1/users/me/data` 选了 `clipboard` 分区时同样删除这些，但保留同步开关和保留天数。清空、删除单条、置顶和保留天数的接口不变。

数据放在 `user_clipboard`（`ciphertext`、`key_id` 两列；去重标签写进原来的 `text_hash`，`text` 写空字符串）、`user_clipboard_settings.key_id`（当前密钥编号）、`user_clipboard_devices` 和 `user_clipboard_wraps`。这一版的迁移删除全部明文条目，旧版本客户端的云剪贴板会显示加载失败或上传失败，直到升级。导出的 `clipboard.json` 只有密文、密钥编号和登记设备的名称与时间，服务端没有明文。`PUT /clipboard/{id}/pin` 和 `PUT /clipboard/devices/{device_id}` 都能匹配 `/clipboard/devices/pin`，`net/http` 的 ServeMux 会把这两个模式判为冲突，所以服务端注册成一个 `PUT /v1/users/me/clipboard/{id}/{action}` 再按路径分开，对外的路径不变。滚动升级的注意事项见 README「多副本部署」。

## 语音贡献

`POST /v1/voice/contributions` 只在用户打开「贡献语音」后由客户端调用，需要用户会话（匿名账号也可以），设备令牌返回 401。multipart：`payload` 为 JSON `{"language","provider","duration_ms","transcript","app_version"}`（未知字段 400，`duration_ms` 1–60000，`transcript` 最多 2000 字符），`audio` 为 WAV（RIFF 结构校验）或 Ogg（起始页签名校验），最多 2 MiB。`duration_ms` 是客户端声明的时长，服务端只校验这个字段的范围，不解析媒体的实际时长；客户端须在上传前按音频采样计算时长，确保不超过 60 秒。返回 201 `{"id"}`。每个用户每小时 30 次（`auth_rates`，所有副本共享）。样本存在 `voice_contributions` 表，只用于改进语音识别，保留 180 天由清理任务删除，注销账号或删除云端数据的 `voice` 分区时一并删除；音频和文本不写日志。

验证码最多尝试五次，每个目标每分钟一次、每小时五次、每天十次，全服务每天最多发送 450 次。邮箱地址统一转为小写。用户接口按客户端地址每分钟最多 120 次。客户端地址默认是 TCP 对端，不信任转发头；部署在反向代理后，把顶层 `client_ip_header` 设为代理覆盖写入的头（Cloudflare 为 `CF-Connecting-IP`；`X-Forwarded-For` 取最后一段），否则同一代理后的所有请求共享此额度。IPv6 按 /64 归为一个地址。官网的 BFF（Cloudflare Pages Functions）代访客调用接口，所有官网访客都从同一组 Cloudflare 出口过来；配置顶层 `site_proxy_secret_env`（指向保存共享密钥的环境变量，值为 32–256 个可见 ASCII 字符，必须与官网 Pages 的 Secret `SITE_PROXY_SECRET` 相同）后，请求头 `X-MSIME-Site-Proxy` 与该密钥相等（常量时间比较）的请求按 `X-MSIME-Client-IP` 中的访客地址计额度（须是合法 IP，IPv6 同样按 /64 归组），优先于 `client_ip_header`；密钥不符、未配置或地址不合法时这两个头被完全忽略。所有按地址的限额都经过同一个计算，所以账号接口、社区接口、匿名开户、遥测、后台登录和词条投稿都随之生效。匿名开户的每日限额（`auth.anonymous.daily_per_address`，默认 5）也按同一个地址计。公开的 `GET /v1/notices` 和 `GET /v1/site/download-mirrors` 另用一份每分钟 1200 次的额度，匿名的 `POST /v1/telemetry/events` 再用一份每分钟 60 次的额度，它们都不占用这 120 次。

本地设置 `docs_enabled: true` 后，Swagger `/swagger/` 包含所有用户接口。生产环境默认关闭文档。未完成生产提供方配置时，不应宣称相应登录已经可用。测试使用本地签名 JWT、模拟短信和 SMTP 服务以及真实 PostgreSQL，不替代生产供应商联调。

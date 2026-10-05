# 用户皮肤社区

社区接口位于 `/v1/community/skins`，与既有 `/v1/skins` 桌面 CSS 目录分开。当前设计格式对应 Apple 自定义键盘 v1，描述颜色、键帽、纹理、渐变及可选 JPEG 壁纸，不接受代码、任意资源 URL 或 CSS。其他平台需实现此设计格式后才能使用。

## 接口

- `GET /v1/community/skins?q=&offset=0&scope=&fields=&category=&include=`：公开目录，按发布时间倒序每页 20 条，返回 `skins` 和 `has_more`。目录不含壁纸字节。`scope=mine` 只列出自己的作品（含被审核员下架的）；`scope=saved` 只列出自己收藏的作品，按收藏时间倒序，分页方式相同，别人已下架的作品不再列出。两者都需要用户会话，否则 401 `user_session_required`；其他 scope 返回 400 `invalid_scope`。`category` 只列出该分类的作品（见下文「键盘皮肤分类」），未知分类返回 400 `invalid_category`。
- `GET /v1/community/skins/{id}?fields=&include=`：公开详情，含完整 design；登录时额外返回自己的评分与是否为作者。
- `POST /v1/community/skins`：需要用户会话，提交 `{id,name,description,design,category?}`。id 为客户端生成的 UUID，用于网络失败后的安全重试。`category` 缺省（或为 null）时为 `other`，未知值或空串返回 400 `invalid_category`。每个账号最多 50 款。响应仍为 `{id}`（首次 201，重试 200），不是作品条目，所以不读 `include`。
- `PATCH /v1/community/skins/{id}?fields=&include=`：仅作者可修改（他人的作品与不存在一样返回 404 `skin_not_found`），提交 `{"category":"<分类>"}`；category 缺省、为 null、空串或未知值都返回 400 `invalid_category`，其他键返回 400 `invalid_json`。返回 200 和与详情形状相同的作品（含完整 design），同样支持 `fields=moderation` 与 `include=category`。不改变审核状态，设为当前分类同样返回 200。被审核员下架的作品作者仍可修改。限流与其他社区接口相同，计入按地址的每分钟额度（超出 429 `rate_limit_exceeded`）。分类见下文「键盘皮肤分类」。
- `POST /v1/community/skins/{id}/download`：需要用户会话，返回 `{design}`，每个账号只计一次下载。
- `PUT /v1/community/skins/{id}/rating`：需要用户会话，提交 `{stars:1..5}`。登录即可评分，不需要先下载；不能给自己的作品评分（403 `download_before_rating_or_own_skin`，错误码沿用旧名，现在只表示「自己的作品」）；不存在或已下架的作品返回 404 `skin_not_found`。重复提交更新同一条评分。
- `PUT /v1/community/skins/{id}/save`：需要用户会话，提交 `{"saved":true|false}` 收藏或取消收藏，重复提交结果相同，返回 200 `{"saved":bool,"saves":int}`（saves 为收藏总数）。不存在或已下架（作者本人除外）的作品收藏时返回 404 `skin_not_found`；取消收藏不看作品状态，总是删除并返回 200，作品下架后用户仍能把它移出收藏。
- `DELETE /v1/community/skins/{id}`：仅作者可删除；不删除其他设备已下载的本地副本。

摘要字段：id、name、description、author、design、downloads、rating_count、rating_average、owned、my_rating，仅在 `fields=moderation` 时出现在自己作品上的 moderation，仅在 `fields=saved` 时出现的 saved（当前用户是否收藏，匿名为 false）和 saves（收藏总数），以及仅在 `include=category` 时出现的 category。列表和详情的 `fields` 是逗号分隔的列表，可以组合，例如 `fields=moderation,saved`；不带 `saved` 时响应与加入收藏之前逐字节相同。人数代表累计去重下载账号数，不代表实时活跃使用人数。发布之后不允许原地替换设计以继承旧版评分；修改设计需发布新作品。

标题最多 32 个 Unicode 字符，说明最多 280 个；设计颜色为 24-bit RGB、数值范围与 iOS 编辑器一致。JSON 请求最大 710,000 字节，壁纸最多 512,000 字节且长宽均不超过 1024。服务器解码后重编码 JPEG，移除原图元数据。未知字段或错误图片会被拒绝。

作者查看自己作品的审核状态：列表和详情带 `fields=moderation` 时，当前用户自己的作品多一个 `moderation` 字段（`approved`、`pending` 或 `removed`）；他人的作品和匿名访问永远不带，也不返回下架原因。不带这个参数时响应与以前逐字节相同，因为已发布的客户端按拒绝未知字段的方式解析。社区是事后审核，`pending` 的作品已经公开，客户端只需对 `removed` 显示「已下架」徽标。其他 `fields` 值返回 400 `invalid_fields`。

发布时名称、描述等文本命中拦截级敏感词返回 422 `blocked_content`（提示「内容包含不允许发布的词语，请修改后再提交」，不要说成服务故障）；敏感词库暂时无法加载时返回 503 `screening_unavailable` 并带 `Retry-After`，作品未保存，稍后重试即可。被封禁的账号返回 403 `account_banned`。

### 键盘皮肤分类

键盘皮肤的图库分类与候选窗皮肤共用同一组 id（`nature`、`guofeng`、`acg`、`cute`、`food`、`tech`、`minimal`、`other`，文案见 [候选窗皮肤 · 分类](#分类)），客户端按 id 显示自己的文案。分类是作品的发布元数据，不写进 design，也不参与 design 校验。

- 分类字段 `category` 只在客户端声明支持时出现：列表、详情和 PATCH 的查询串带 `include=category` 时，每个条目都带 `"category": "<分类>"`；不带时响应与引入分类之前逐字节相同。`include` 只接受空值和 `category`，其他值返回 400 `invalid_include`。`include=category` 与 `fields=moderation` 互相独立，可以同时使用。
- 发布时用 `category` 指定，缺省为 `other`；引入分类之前发布的作品迁移后都是 `other`。分类不参与发布重试的比较：同一 id、内容相同而分类不同的重试仍返回 200，保留已存的分类，之后改分类用 PATCH。
- 作者用 `PATCH /v1/community/skins/{id}` 修改，审核员在管理后台修改（`set_skin_category`，见 [管理后台](admin.md)）。
- 滚动升级期间旧版本副本仍在服务：它们忽略 `include` 和 `category` 查询参数（响应不带分类、列表不筛选），会以 400 `invalid_json` 拒绝发布请求体里的 `category` 键，且没有 PATCH 路由（返回纯文本的 405，不是 JSON 错误体）。客户端应把缺少的 `category` 当作 `other`，并在所有副本升级之后再在发布请求体中发送 `category` 或调用 PATCH。

加入分类的版本在启动时给 `community_skins` 增加 `category text NOT NULL DEFAULT 'other'`（已有行为 `other`，PostgreSQL 11 起带常量默认值的加列不重写表）、命名约束 `community_skins_category_check` 和服务按分类筛选的索引 `community_skins_category_newest (category, created_at DESC, id)`，重复执行不会改变任何东西；启动探测会检查这一列，缺列时自动执行迁移。没有新表，所以不需要新的授权：按最小权限部署时只需在上线前用迁移账号执行新版本的 `-migrate-users`，运行角色对这张表已有的 SELECT/INSERT/UPDATE/DELETE 覆盖新列。旧二进制插入的行取默认值 `other`，回滚不需要处理这一列。

## 账号与 K8s

复用现有 PostgreSQL 用户体系。配置 `auth.apple.client_ids: ["app.msime.ios"]`；Apple Developer 中为相同 App ID 启用 Sign in with Apple，并重新生成包含该 entitlement 的签名配置。客户端不包含设备共享令牌或 Apple 私钥。Apple nonce 来自后端挑战，ID Token 校验继续使用现有 issuer/audience/signature/nonce 校验。

运行角色有 DDL 权限时不需要单独迁移：新版本启动发现缺表会自己补上。按最小权限部署（运行角色只有 DML）时仍照旧：上线前使用迁移账号执行新版本的 `-migrate-users`，或者由数据库管理员在事务中执行 `internal/account/community_schema.sql`，并给运行角色授予三张新表的 SELECT/INSERT/UPDATE/DELETE。加入收藏的版本新增 `community_skin_saves`（主键 `(skin_id,user_id)`，`created_at` 为收藏时间，索引 `community_skin_saves_user (user_id, created_at DESC)`，对作品和账号都级联删除），最小权限部署时同样要授权：`GRANT SELECT, INSERT, UPDATE, DELETE ON community_skin_saves TO msime_backend;`。这些表放在现有 PostgreSQL 中，无需 K8s 本地目录或 PVC。先迁移再滚动更新，旧二进制可兼容新增表。数据库需按现有方案备份。

下载、收藏及评分的唯一键保证多副本并发去重；发布锁定账号行保证配额。浏览和写入继续使用数据库限流。没有给下载用户数设置产品上限；实际吞吐需按部署容量测试，不能把配额或副本数解释为可承载人数。

Apple 客户端入口：皮肤 → 皮肤社区。用户显式确认公开素材后发布，浏览不会修改当前皮肤。账号注销级联删除作品、评分、收藏和下载记录。登录令牌保存到本机 Keychain，刷新串行执行。

## 初始精选皮肤

`assets/community-starter-skins.json` 包含 8 款项目自有的纯参数设计，供社区冷启动。`scripts/community_seed.py` 只生成 SQL，不连接数据库：

```sh
python3 scripts/community_seed.py > /tmp/community-starter.sql
psql -X -v ON_ERROR_STOP=1 "$MSIME_MIGRATION_DATABASE_URL" -f /tmp/community-starter.sql
```

SQL 在单个事务中切换到既有 DML 运行角色 `msime_backend`，使用固定 ID 和事务锁保证重复执行安全；已有 ID 的内容不一致时整个事务失败，不覆盖用户作品或已获评分的设计。执行前审核目录和 SQL，并确认目标数据库。

作者「水杉精选」是专门标记项目精选内容的非交互发布主体，不是虚构的普通用户。只创建作者记录，不创建登录身份、密码、令牌或会话；如其 ID 已绑定登录身份或会话则拒绝执行。脚本只写作者和皮肤，不创建下载或评分。后续改版应新增版本 ID，避免继承旧版评分。

### AI 插画抽卡任务

客户端使用 `POST /v1/skins/jobs` 提交内部生成的原创场景描述，收到 202 后每 5 秒调用 `GET /v1/skins/jobs/{job}`。`running` 表示仍在绘制，`succeeded` 的 `artwork` 包含与同步接口相同的有界 PNG/JPEG 数据，`failed` 表示此次生成失败。无需让一个公开 HTTP 请求等待完整生图时间。

每个认证主体最多保留 3 个任务，全局最多 `min(8, max_concurrent)` 个；领取、取消或失败后调用 `DELETE /v1/skins/jobs/{job}` 释放任务。超出上限或服务正在关闭时返回 503 `skin_jobs_busy`；任务存储（数据库）暂不可用时提交、查询和删除返回 503 `job_unavailable`。两者都带 `Retry-After: 5`，客户端稍后再试即可。任务绑定认证主体，令牌刷新不改变归属，其他主体查询和删除都返回 404。上游请求最多运行 180 秒，服务关闭会取消并等待任务，被取消的任务以 `failed`、`reason` 为 `cancelled` 结束。提交请求不应自动重试，以免重复生成。启用数据库时每个认证主体每天最多提交 10 个任务（匿名账号同样适用，删除的任务也计数；`auth_rates` 键 `skin-art-day:<owner 摘要>`，所有副本共享），超出返回 429 `rate_limit_exceeded`，`Retry-After` 是配额窗口的剩余秒数。

这些是临时草稿，10 分钟后失效；不写入社区或用户皮肤库。客户端应提示重新抽取，只有用户保存后才成为持久化皮肤。

多副本行为（启用 `auth`、有 PostgreSQL 时）：

- 任务、状态和生成结果存在共享表 `skin_jobs`（`internal/account/skin_job_schema.sql`），结果按同步接口的 JSON 原样存为 bytea，单条最多约 12 MB。接受提交的副本负责调用上游并把结果写回表中；`GET` 和 `DELETE` 落在任何副本上都看到同一个任务，重启其他副本不影响任务。
- 上面两个上限按整个部署计算，不随副本数翻倍：创建任务时在一个 advisory lock 下统计表中未过期、且执行副本仍在心跳（或已结束）的任务再插入。`max_concurrent` 取接受请求那个副本的配置，各副本应配置相同的值。
- 执行任务的副本每 5 秒刷新一次心跳，同一条语句读取取消标记。`DELETE` 落在执行副本上立即中止上游请求；落在其他副本上时先标记取消，执行副本最迟在下一次心跳（约 5 秒）中止上游请求并删除该行，在此之前该任务仍计入上限，但对 `GET` 已经是 404。如果执行副本在删除该行之前就被强制终止，该行心跳超过 30 秒后不再计入上限，并在下一次创建任务时被删除。
- 副本正常关闭（SIGTERM）时取消自己执行的任务并写回 `failed`/`cancelled`。副本被强制终止、OOM 或与数据库断开超过 30 秒时心跳停止，其他副本把该任务报告为 `failed`/`cancelled`；即使原副本之后恢复，结果也不会再覆盖这个状态。这类任务不再计入上限，客户端不调用 `DELETE` 也不会让死去副本留下的任务占满上限；`DELETE` 直接删除记录，否则保留到过期供 `GET` 读取。
- 过期任务对所有副本立即不可见；每次创建任务时顺带删除过期行，后台每小时的清理也会删除。表中同时最多 8 条未过期任务，约 100 MB，需计入数据库容量。
- 新表随启动迁移自动创建，启动检查会探测该表。按最小权限部署时，先用迁移账号执行新版本的 `-migrate-users`，再授予运行角色权限：`GRANT SELECT, INSERT, UPDATE, DELETE ON skin_jobs TO msime_backend;`。滚动更新期间旧版本副本仍在进程内保存任务，新旧副本之间互相看不到对方的任务，客户端轮询可能短暂得到 404 并提示重新抽取；全部副本更新后恢复正常。

没有数据库（`auth.enabled=false`）时仍使用进程内实现：任务只在接受它的进程内可见，重启即失效，上限按进程计算，只适合单副本部署。

## 候选框皮肤包入库

候选框皮肤包（msime-skins 格式，`skin.toml` 加图片等资源）直接存入 PostgreSQL，通过既有 `/v1/skins` 目录下发，客户端不必固定某个 msime-skins 提交。格式、校验和冲突规则见 `internal/skins/README.md`。

部署：运行角色有 DDL 权限时新版本启动会自动建表。按最小权限部署时，上线前用迁移账号执行新版本的 `-migrate-users`，或在事务中执行 `internal/account/candidate_skin_schema.sql`（需要 PostgreSQL 12 及以上，用到生成列），并给运行角色授予两张新表的权限：

```sql
GRANT SELECT, INSERT, UPDATE, DELETE ON candidate_skins, candidate_skin_resources TO msime_backend;
```

服务只需要 SELECT；INSERT 供下面的种子 SQL 以运行角色写入，UPDATE/DELETE 供运维下架（`UPDATE candidate_skins SET published = false, updated_at = now() WHERE id = '…'`）或删除（级联删除资源）。先迁移再滚动更新，旧二进制不读这两张表。

`scripts/candidate_skins_seed.py` 只生成 SQL，不连接数据库：

```sh
python3 scripts/candidate_skins_seed.py ~/src/msime-skins > /tmp/candidate-skins.sql
python3 scripts/candidate_skins_seed.py ~/src/msime-skins --only bigfish,qq-blue > /tmp/candidate-skins.sql
psql -X -v ON_ERROR_STOP=1 "$MSIME_MIGRATION_DATABASE_URL" -f /tmp/candidate-skins.sql
```

脚本按客户端规则校验每个包，客户端会拒绝的清单直接拒绝（不改写），并在 stderr 说明原因；任何一个包被拒绝时不输出 SQL。`license.assets` 含 `UNVERIFIED` 的包（如 `niya-demo`）默认跳过，用 `--only` 点名时拒绝。非皮肤资源（README 等）不入库，会在 stderr 列出；符号链接、超过 4 MiB 的文件、超过 16 MiB 或 512 个条目的包直接拒绝。SQL 注释记录来源仓库（去掉凭据）和提交，工作区有未提交改动时给出警告。

SQL 在单个事务中切换到运行角色 `msime_backend`（`--role` 可改），加事务锁，只插入缺失的行；同一 ID 已存在且清单或资源集合（路径与 SHA-256）不一致时整个事务失败，不覆盖已发布的包。内容相同时重复执行不改变任何行。改版应发布新 ID，或由运维先显式下架、删除旧包。执行前审核 SQL 并确认目标数据库。

msime-skins 清单写 `base = "fluent"`（msime-windows 只接受四个内置 ID）。客户端把清单里的 `fluent` 当作 `system` 的别名，脚本和服务端同样接受；数据库保存原始清单字节，`/v1/skins` 返回的 `base` 为 `system`。其余 Windows 内置 ID 作为 base 仍被拒绝。

## 候选窗皮肤包分享

用户可以把自己的候选窗皮肤包（msime-skins 格式，`skin.toml` 加 PNG/JPEG 图片）公开发布到社区，其他用户下载后安装到外部皮肤目录。同一张表也是账号的皮肤库：登录后客户端把本机外部皮肤以私有（`private`）作品同步上来，私有作品只有作者本人能看到、预览和下载，作者可以随时把它转为公开。接口位于 `/v1/community/candidate-skins`，数据存放在 `community_candidate_skins`、`community_candidate_skin_files`、`community_candidate_skin_downloads`、`community_candidate_skin_ratings` 和 `community_candidate_skin_saves` 五张独立的表中，与上面的精选 `candidate_skins` 表和 `/v1/skins` 目录互不影响：用户上传不会进入 `/v1/skins`。

### 接口

- `GET /v1/community/candidate-skins?q=&offset=0&scope=&fields=&category=&include=`：公开目录，按发布时间倒序每页 20 条，返回 `skins` 和 `has_more`，不含清单和图片字节。`scope=mine` 只列出自己的作品；`scope=saved` 只列出自己收藏的作品，按收藏时间倒序。两者都需要用户会话，否则 401 `user_session_required`；其他 scope 返回 400 `invalid_scope`。只有 scope 非空且 `fields` 含 `sync` 时才会包含自己的私有作品，其余情况只列公开作品；别人的私有作品永远不出现，收藏之后被作者设为私有的作品也会从收藏者的列表里消失。`category` 只列出该分类的作品（见下文「分类」），未知分类返回 400 `invalid_category`。
- `GET /v1/community/candidate-skins/{id}?fields=&include=`：公开详情；登录时额外返回自己的评分与是否为作者。私有作品只有作者带 `fields=sync` 时可见，其他情况与不存在一样返回 404 `skin_not_found`。
- 列表和详情的 `fields` 是逗号分隔的列表，取值 `sync`、`moderation`、`saved`，可任意组合，例如 `fields=sync,moderation`、`fields=sync,saved`。`moderation` 和 `saved` 的含义与用户皮肤相同（见上文），单独使用时不会带出同步字段。
- `GET /v1/community/candidate-skins/{id}/preview`：返回预览图 `{path,content_type,data}`，data 为标准 base64，content_type 为 `image/png` 或 `image/jpeg`。公开作品任何人可取，私有作品仅作者可取，否则 404。
- `POST /v1/community/candidate-skins`：需要用户会话，提交 `{id,name,description,manifest,files,visibility?,category?}`。manifest 为原样的 skin.toml 文本，files 的键为包内相对路径、值为标准 base64；`visibility` 为 `public` 或 `private`，缺省为 `public`，其他值返回 400 `invalid_visibility`；`category` 缺省（或为 null）时为 `other`，未知值返回 400 `invalid_category`。首次发布返回 201，同一请求重试返回 200。
- `GET /v1/community/candidate-skins/sync`：需要用户会话（否则 401 `user_session_required`），返回 `{"skins":[{id,package_id,request_sha256,visibility,updated_at}]}`：自己的全部作品（含私有），按 `updated_at` 倒序，不分页（每个账号最多 100 款）。
- `PUT /v1/community/candidate-skins/{id}`：仅作者可替换（否则 404 `skin_not_found`），提交 `{name,description,manifest,files}`，校验、大小上限、超时和图片重新编码都与发布相同。清单 `id` 必须与原作品的 `package_id` 相同，否则 409 `candidate_skin_package_mismatch`；公开作品的新清单必须声明 `[license] assets`，否则 400 `candidate_skin_license_required`。替换清单、图片、标题、说明、version、license 和预览图，重新计算 `request_sha256` 并更新 `updated_at`；保留 id、可见性、created_at、下载与评分。与已存 `request_sha256` 相同时直接返回 200，不写入也不计限流。
- `PATCH /v1/community/candidate-skins/{id}`：仅作者可修改（否则 404），提交 `{"visibility"?: "public"|"private", "category"?: "<分类>"}`，两个键至少带一个（都缺省返回 400 `invalid_visibility`），在同一事务里生效，返回 200 和作品。转为公开要求已存的 license assets 非空（否则 400 `candidate_skin_license_required`），占用公开配额（满额 409 `candidate_skin_publish_limit`），并计入每小时发布限流；切换可见性会更新 `updated_at`，已有下载与评分保留。修改分类（未知值 400 `invalid_category`）计入私有创建与替换共用的每小时 60 次，不更新 `updated_at`、不改 `request_sha256`，也不让作品重新进入审核。设为当前值时不做任何修改，也不计限流。
- `POST /v1/community/candidate-skins/{id}/download`：需要用户会话，返回 `{id,package_id,manifest,files}`，每个账号只计一次下载。私有作品仅作者可下载，否则 404。
- `PUT /v1/community/candidate-skins/{id}/rating`：需要用户会话，提交 `{stars:1..5}`。登录即可评分，不需要先下载；不能给自己的公开作品评分（403 `download_before_rating_or_own_skin`，错误码沿用旧名，现在只表示「自己的作品」）；不存在、私有或已下架的作品返回 404 `skin_not_found`。重复提交更新同一条评分。
- `PUT /v1/community/candidate-skins/{id}/save`：需要用户会话，提交 `{"saved":true|false}`，重复提交结果相同，返回 200 `{"saved":bool,"saves":int}`。不存在、已下架（作者本人除外）或别人的私有作品收藏时返回 404 `skin_not_found`；取消收藏总是返回 200；作者可以收藏自己的私有作品。
- `DELETE /v1/community/candidate-skins/{id}`：仅作者可删除，连带删除图片、下载、评分和收藏记录；不删除其他设备已安装的副本。

摘要字段：id、package_id、name、description、author、version、license（code、assets、source，缺省为空字符串）、size（重新编码后的图片总字节数）、file_count（图片数量，不含 skin.toml）、downloads、rating_count、rating_average、owned、my_rating、created_at，以及仅在 `fields=saved` 时出现的 saved 和 saves。

同步字段 `visibility`、`updated_at` 和 `request_sha256`（上传请求原始字节的摘要，仅作者可见，其他人看到的作品不带该字段）只在客户端声明支持时出现：列表和详情带 `fields=sync`（其他值返回 400 `invalid_fields`），发布请求体带 `visibility` 键，以及 sync、PUT、PATCH 三个接口。已发布的客户端按拒绝未知字段的方式解析摘要，也会拒绝 license assets 为空的条目，所以没有声明支持的响应与引入私有作品之前逐字节相同，并且永远不含私有作品。

分类字段 `category` 同样只在客户端声明支持时出现：列表、详情、发布（POST）、替换（PUT）和 PATCH 的查询串带 `include=category` 时，每个条目都带 `"category": "<分类>"`；不带时响应与引入分类之前逐字节相同。`include` 只接受空值和 `category`，其他值返回 400 `invalid_include`。`include=category` 与 `fields=sync` 互相独立，可以同时使用。

### 分类

分类是作品的发布元数据，不写进 skin.toml，也不参与清单校验。取值固定为以下 8 个，客户端按 id 显示自己的文案：

| id | 名称 |
| --- | --- |
| `nature` | 自然 |
| `guofeng` | 国风 |
| `acg` | 二次元 |
| `cute` | 可爱 |
| `food` | 美食 |
| `tech` | 科技夜色 |
| `minimal` | 简约 |
| `other` | 其他 |

- 发布时用 `category` 指定，缺省为 `other`；引入分类之前发布的作品迁移后都是 `other`。
- 分类不计入 `request_sha256`：内容相同、分类不同的重试仍是同一请求，返回已存的作品和它原来的分类。之后改分类用 PATCH。已在私有库同步的包转为公开时走 PUT 替换加 PATCH，分类也在这次 PATCH 里设置。
- 作者用 PATCH 修改，审核员在管理后台修改（`set_candidate_skin_category`，见 [管理后台](admin.md)）。两者都不改 `updated_at`，所以不会让同步客户端以为包内容变了。
- 滚动升级期间旧版本副本仍在服务：它们忽略 `include` 和 `category` 查询参数（响应不带分类、列表不筛选），但会以 400 `invalid_json` 拒绝请求体里的 `category` 键。客户端应把缺少的 `category` 当作 `other`，并在所有副本升级之后再在请求体中发送 `category`。

### 限制与校验

- 只接受 `skin.toml` 加 PNG/JPEG 图片（扩展名 png、jpg、jpeg，不区分大小写），最多 3 个图片文件，每个文件都必须被清单引用：只能是 `preview`、`candidate_window.decoration.image` 和 `candidate_window.background.image`。不接受样式表（`toolbar_stylesheet`）、字体、SVG、GIF 或 WebP；Go 标准库不能重新编码 WebP，支持它需要新增依赖。
- 单个图片不超过 1 MiB、合计不超过 2 MiB，上传时和重新编码后都要满足；skin.toml 另计，最多 65,536 字节。每边 1 到 2048 像素，整包解码像素合计不超过 800 万，尺寸在完整解码前从文件头读取。每张 JPEG 最多 32 个扫描段（SOS）：Go 的解码器对每个扫描都要遍历整张图，且不限制扫描数，常见渐进式 JPEG 约 10 个扫描；超过的返回 `candidate_skin_image_invalid`。
- 必须用 `preview` 指定一张包内的 PNG/JPEG 作为预览图，重新编码后不超过 256 KiB。
- 公开作品的 `skin.toml` 必须包含 `[license]` 且 `assets` 非空；私有作品可以省略，转为公开前需先用 PUT 补上。客户端发布时还要求用户勾选确认拥有素材权利。清单的 `version` 和 `[license]` 的 `code`、`assets`、`source` 不能含控制字符（包括换行和制表符），否则返回 `invalid_candidate_skin_package`：客户端会拒绝含控制字符的列表项，一条这样的记录会让所在的整页列表失败。
- 路径规则与客户端 `safe_resource` 一致：相对路径，每段只含 `A-Za-z0-9._-`，不允许空段、`.`、`..` 或反斜杠；键不能是 `skin.toml`，转小写后不能重复。
- 清单用与客户端加载器一致的规则校验（`internal/skins/client.go` 的 `ParseStored`），包括 ID 格式、保留主题 ID 与四个内置 ID、schema_version、base、supports 和窗口参数。
- 服务器按扩展名选择解码器解码每张图片后重新编码（PNG 最高压缩、JPEG 质量 90），因此会去除 EXIF、XMP、ICC 和文本块，APNG 只保留第一帧；扩展名与内容不符的图片被拒绝。去掉 ICC 可能带来轻微色差，Go 的 PNG 编码器也可能让已优化的 PNG 变大，接近上限的包可能在重新编码后被拒绝。
- 标题最多 32 个 Unicode 字符，说明最多 280 个，均先去除首尾空白；与清单里的 `name` 无关。标题不能含控制字符，说明只允许换行和制表符两种控制字符。发布请求最多 3,200,000 字节。
- 每个账号最多 100 款（超出 409 `candidate_skin_library_limit`），其中公开最多 20 款（超出 409 `candidate_skin_publish_limit`）。公开发布和转为公开共用每小时 10 次，私有创建和 PUT 替换共用另一份每小时 60 次（均按账号计，数据库限流）；图片解码每个进程最多同时 2 个，繁忙时返回 503 `candidate_skin_busy`。
- 官方发布账号：`auth.community.official_skin_publishers` 列出项目方用来发布官方皮肤的账号，值为用户 ID（`auth_users.id`，64 位小写十六进制，即登录后 `user.id` 的值），默认为空，最多 50 个且不能重复，格式不对时服务拒绝启动（用户体系未启用时也会校验）。名单中的账号不受公开 20 款的限制，公开作品只受每账号 100 款的总数上限约束；公开发布与转为公开、私有创建与 PUT 替换（含改分类）两份每小时额度都放宽到 200 次，仍按账号计，作为配置失误或客户端循环重试时的上限。其余规则不变：作品照常进入事后审核（`pending`），包校验、大小上限、公开作品的 license 要求都一样，公开接口返回的作品与普通账号的作品没有区别。名单只在服务启动时读取，修改后需滚动重启。旧版本不认识 `auth.community`，而配置中的未知字段会让服务拒绝启动：先让所有副本都运行包含这个配置项的版本，再把它写进生产配置并滚动重启；回退到更早的版本之前，先从配置中删除 `auth.community`。

错误码：400 `invalid_json`、`invalid_skin_metadata`、`invalid_community_id`、`invalid_visibility`、`invalid_fields`、`invalid_category`、`invalid_include`、`invalid_candidate_skin_package`、`candidate_skin_file_type`、`candidate_skin_file_path`、`candidate_skin_too_large`、`candidate_skin_image_invalid`、`candidate_skin_license_required`、`candidate_skin_preview_required`；404 `skin_not_found`；409 `candidate_skin_id_conflict`、`candidate_skin_publish_limit`、`candidate_skin_library_limit`、`candidate_skin_package_mismatch`；422 `blocked_content`（发布与替换）；429 `rate_limit_exceeded`；503 `candidate_skin_busy`、`screening_unavailable`（带 `Retry-After`）、`auth_unavailable`。

### ID 与版本

`id` 是客户端生成的发布 UUID，用于网络失败后的安全重试：同一账号用相同内容重试返回原作品，内容不同或属于其他账号返回 409。已提交作品的重试在每小时发布限流之前就会返回，不计入次数，因此丢失响应后的重试不会变成 429。两个账号同时用同一 UUID 发布时，后提交的一方同样得到 409。`package_id` 是清单里的 `id`，也是客户端安装的目录名。服务端不改写 skin.toml。不同作者可以发布相同的 `package_id`，安装时会替换本机同名皮肤，客户端会先请求确认。作者可以用 PUT 原地替换同一 `package_id` 的新版本，id、下载与评分保留；换成另一个 `package_id` 需要用新的 UUID 发布新作品。

### 部署与审核

运行角色有 DDL 权限时新版本启动会自动建表。按最小权限部署时，上线前用迁移账号执行新版本的 `-migrate-users`，或在事务中执行 `internal/account/community_candidate_skin_schema.sql`（需要 PostgreSQL 12 及以上，用到生成列），并给运行角色授予四张新表的权限：

```sql
GRANT SELECT, INSERT, UPDATE, DELETE ON community_candidate_skins, community_candidate_skin_files, community_candidate_skin_downloads, community_candidate_skin_ratings, community_candidate_skin_saves TO msime_backend;
```

加入收藏的版本新增 `community_candidate_skin_saves`（主键 `(skin_id,user_id)`，`created_at` 为收藏时间，索引 `community_candidate_skin_saves_user (user_id, created_at DESC)`，对作品和账号都级联删除）；启动探测会检查这张表，缺表时自动执行迁移。已有部署只需额外授予这一张表，见上面的 GRANT。旧二进制不读这张表，回滚不需要处理它。

先迁移再滚动更新，旧二进制不读这四张表。加入私有作品的版本在启动时给 `community_candidate_skins` 增加 `visibility`（已有行为 `public`）和 `updated_at`（已有行取其 `created_at`）两列，并把 license assets 的列约束换成命名约束 `community_candidate_skins_license_check`（公开行要求非空，私有行可为空），重复执行不会改变任何东西；旧二进制仍能读写这张表。但旧二进制不认识 `visibility`，会把私有作品当公开作品列出和下发，所以必须等所有副本都换成新版本后再让客户端开始同步私有作品，回滚到旧版本前也要先处理私有行。

加入分类的版本在启动时给 `community_candidate_skins` 增加 `category text NOT NULL DEFAULT 'other'`（已有行为 `other`，PostgreSQL 11 起带常量默认值的加列不重写表）、命名约束 `community_candidate_skins_category_check` 和服务按分类筛选的索引 `community_candidate_skins_category_newest (category, created_at DESC, id)`，重复执行不会改变任何东西；启动探测会检查这一列，缺列时自动执行迁移。没有新表，所以不需要新的授权：按最小权限部署时只需在上线前用迁移账号执行新版本的 `-migrate-users`，运行角色对这张表已有的 SELECT/INSERT/UPDATE/DELETE 覆盖新列。旧二进制插入的行取默认值 `other`，回滚不需要处理这一列。每个账号满额时约占 200 MiB bytea（100 款，每款最多 2 MiB 图片），需计入数据库容量与备份。账号注销级联删除作品、图片、评分、收藏和下载记录。

公开作品发布即公开，不做事前审核；候选皮肤和其他社区内容一样由管理后台的社区审核页事后复核，审核规则见 [管理后台的「社区事后审核」](admin.md#社区事后审核)。管理后台 API 另外提供 `GET /api/candidate-skins`（可用 `visibility=public|private` 和 `category=<分类>` 筛选）、`GET /api/candidate-skins/{id}`（元数据、可见性、分类、清单文本和每个文件的路径、大小、SHA-256，不含图片字节）、审计过的 `delete_candidate_skin` 永久删除操作，以及审计过的 `set_candidate_skin_category` 用于修改分类；社区审核页的候选皮肤详情抽屉提供分类下拉框。

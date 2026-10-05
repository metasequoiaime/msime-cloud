# AGENTS.md

给在本仓库（水杉云 msime-cloud）工作的编码代理的约定。Claude Code 通过 `CLAUDE.md` 引入本文件，Codex 直接读取本文件，两边共用这一份，不要另起副本。接口、部署和各功能的细节以 [README](README.md) 和 [docs/](docs) 为准，这里只写不读代码就容易踩错的操作约定。

## 语言

本仓库的以下文本一律使用中文：

- 代码注释：新增或修改的注释，包括 Go、TypeScript、Python、SQL、YAML 和 shell 中的注释与文档注释。
- Pull Request：标题和正文。
- 评论：PR 评论、issue 评论，以及对他人评论的回复。
- Code review：review 总结和每一条行内评论。

说明：

- 本规则优先于代理全局配置中「GitHub 上的文本用英文」之类的约定，只作用于本仓库。
- 代码中的标识符、命令、路径、错误码、日志字段和接口字段保持原样，不翻译；中文句子里引用它们时用反引号括起来。
- 修改已有的英文注释时，把改动到的那条注释改写成中文；不要为了改语言而批量重写没有改动的代码。
- PR 标题保留英文的 Conventional Commits 类型前缀，冒号后面写中文，例如 `fix(account): 修复缺少 R2 密钥时启动失败`。前缀不能翻译：PR 按 squash 合并，标题就是 main 上的提交标题，`scripts/release/backend.py` 只认 `feat:`、`fix:`、`type!:` 这类英文前缀来决定版本号。
- commit message 不在本规则范围内，保持现有的英文 Conventional Commits。

## 仓库结构

- `cmd/msime-server`：服务端入口；`cmd/msime-cloud`：给人和 AI 助手用的命令行（见 README「命令行」）。
- `internal/server`：HTTP 服务、在线输入接口、管理后台路由与鉴权；`internal/account`：用户体系、社区、管理后台数据，依赖 PostgreSQL；`internal/engine`：引擎进程查询；`internal/githubapp`：管理后台与官网词条投稿使用的 GitHub App 客户端；`internal/skins`：内置皮肤目录；`internal/contract`：由契约生成的常量。
- `admin-web/`：管理后台前端，`dist/` 是提交进版本库的构建产物，由 `admin-web/embed.go` 嵌入服务端。
- `contracts/protocol.json`：客户端接口的权威来源。它原是 MSIME-Engine `contracts/backend/protocol.json` 的副本，Engine 归档后由本仓维护；改语义要同时顾及已发布的客户端。
- `native/`、`third_party/`：引擎进程说明与 msime 主仓库子模块。引擎（`msime-backend-engine`）和词库锁文件都来自子模块，只在 Docker 镜像和原生集成测试中构建。

## 验证

改完代码在本地跑与 CI 相同的检查，不要只跑改动的那个包：

```sh
python3 scripts/sync_contract.py --check
python3 scripts/generate_openapi.py --check
python3 -m unittest discover -s scripts/release -p "test_*.py"
go test -race ./...
go vet ./...
gofmt -l ./cmd ./internal
```

- 依赖 PostgreSQL 的测试在没有 `MSIME_TEST_DATABASE_URL` 时会跳过，`go test` 显示 ok 不代表它们跑过。改动 `internal/account` 或 `internal/server` 中涉及数据库的代码时，本地起一个 PostgreSQL 并设置该变量再跑这两个包，CI 的「用户体系 PostgreSQL 集成测试」用的是 `postgres:17-alpine`。
- `go test` 在 CI 中同时跑 ubuntu 和 windows。测试不能假设 Unix 专有行为，例如文件权限位（Windows 上 `0600` 读回来是 `0666`）、路径分隔符或 shell 命令；确实只在 Unix 上成立的断言要按 `runtime.GOOS` 区分并写明原因。
- CI 有 Go 语句覆盖率门禁（`scripts/check_go_coverage.py`）：全仓、`internal/account`、`internal/server` 各自不低于 90%。新增的代码要带测试；新增可执行包时，同时把它加进该脚本要求出现的包清单及 `scripts/test_go_coverage.py`。
- 改动 `admin-web/` 后执行 `pnpm --dir admin-web lint` 和 `pnpm --dir admin-web build`，把重新生成的 `dist/` 一起提交，CI 会用 `git diff --exit-code -- admin-web/dist` 检查。后台的 CSP 限制和冒烟测试见 [admin-web/README.md](admin-web/README.md)。
- Docker 构建只复制 `go.mod`、`go.sum`、`cmd`、`internal`、`VERSION`、`version.go` 和 `admin-web`（见 `Dockerfile`）。服务端不能依赖这些路径之外的文件：新增的 `go:embed` 资源要放在会被复制的目录里，或者像 `docs/docs.go` 那样放进只有命令行引用的独立包。

## 接口与契约

- 新增或修改 `/v1` 接口后运行 `python3 scripts/generate_openapi.py` 更新 `internal/server/swagger/openapi.json`。`routes_contract_test.go` 会对规范里的每个接口检查鉴权和来源限制，命令行的 `routes`、`describe` 也直接读这份规范。
- 共享接口的变化改 `contracts/protocol.json`，再用 `scripts/sync_contract.py` 重新生成常量。
- 管理后台 `/api/*` 的路由由表分发：`internal/account/admin_actions_registry.go` 的 `adminRoutes`、`adminLists`，以及 `internal/server/admin.go` 的 `adminServerRoutes`。新增路由在对应的表里登记，处理函数写在各功能自己的文件中。请求体、权限和返回格式写进 [docs/admin.md](docs/admin.md)，命令行的 `describe` 会引用其中提到该路由的段落。
- 错误统一返回 `{"error":{"code":"...","message":"..."}}`，不要把上游服务商的响应正文、凭据或用户输入透传给客户端，也不要写进日志。

## 配置、数据库与多副本

- 配置中的未知字段会让服务拒绝启动。新增配置字段时要考虑滚动升级：旧版本副本不认识新字段，所以新字段要等所有副本都运行新版本后才能写进生产配置；回退版本前先从配置删除新字段。在 README 写明上线顺序，参照「多副本部署」一节对 `replicas` 的写法。
- 服务启动时会在 advisory lock 下自动补建缺失的表。新增表或列时，同时考虑最小权限部署：运行账号只有 DML 权限时需要先用有 DDL 权限的账号执行 `-migrate-users`，并在 README 列出需要授予运行角色的新表。
- 生产是多副本部署（同一个 PostgreSQL，Cloudflare Tunnel 轮询转发，无粘性会话）。需要跨请求共享的状态（限流、任务、锁、去重）放在 PostgreSQL 里，不要只存在进程内存中；确实只能按副本计算的上限要在 README「多副本部署」中说明。

## 发布与部署

- 合并到 `main` 的相关改动会由「后端版本与镜像发布」工作流自动递增 `VERSION`、打 `backend-v*` 标签、构建镜像，并把 `msime-cloud` 命令行附到 GitHub Release；只改 Markdown 不会发布。不要手动修改 `VERSION` 或打标签。
- 生产部署由 yldm-platform 的 ImageUpdater 跟踪新镜像，本仓库不直接操作 Kubernetes。排查线上问题时可以只读查看集群，任何修改都先问用户。

## 安全与改动范围

- 不提交凭据、真实用户数据或生产配置：`config.json`、`.env`、`.env.admin.local`、`config.admin.local.json` 已在 `.gitignore` 中。测试和示例只用合成数据，邮箱用 `example.com` / `example.test` 这类保留域名。
- 只暂存本次改动相关的路径，不用 `git add -A`；不顺手重构或清理与任务无关的代码。

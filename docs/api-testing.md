# API 回归测试

范围以当前 OpenAPI 的 **106 个 method + path 操作**为准，另登记 **26 个后台与文档操作**（自动支持的 HEAD、文档静态资源在对应文档用例中验证）。逐接口的业务测试索引见 [api-coverage.json](../internal/server/testdata/api-coverage.json)。索引中的引用指向真实测试函数，不以文件名或覆盖率代替行为验证。

## 覆盖内容

- 公共路由：所有已公布操作的匿名/错误凭据响应、账号系统禁用响应、跨域拒绝、错误方法与同源预检。社区浏览是公开的，测试保留其匿名读取约定，私人数据与写入仍需用户会话。
- 账号：验证码登录、验证码重放/发送失败、OIDC 签名与声明校验、资料读写、令牌轮换及旧令牌重放、单会话/全部退出、账号删除与用户隔离。所有公开声明为 JSON 的账号写接口检查畸形 JSON、尾随 JSON、未知字段、错误媒体类型。
- 用户数据：偏好版本冲突，剪贴板授权/查询/单条与全部删除，四类词库 CRUD/导入/导出/分页/修订冲突/所有权，候选排序、置顶、删除、快照恢复及原子性。依赖词库语义的测试调用真实固定版本原生引擎。
- 输入与 AI：各输入操作的成功结果和非法请求，目录搜索/分页，聊天/翻译/音频请求转换、供应商失败、超时、凭据隔离，WebSocket 音频中继/限流/会话过期，图片生成与异步任务的归属、取消、容量及失败状态。供应商协议测试使用本地 HTTP/TLS/WebSocket 服务，不消耗生产配额。
- 社区、遥测与后台：发布/浏览/下载/评分/收藏/应用/删除，幂等性与关联记录清理；后台数据筛选/详情/操作审计、单会话撤销、管理员添加、启用、停用与会话撤销、Google 登录与 CSRF。遥测重复事件不覆盖原记录，非法记录不得落库，失败管理操作不得生成成功审计。

## 完整运行

必须使用独立的 PostgreSQL 数据库，名称包含 `msime_auth_test`；账号测试会清空测试表。不要复用本地开发或生产数据库，也不要并行运行多个使用同一测试数据库的 `go test` 进程。

```sh
git submodule update --init
(cd third_party/msime && cargo build -p msime-backend-engine --release --locked)
install -D third_party/msime/target/release/msime-backend-engine bin/native/msime-engine
python3 scripts/fetch_engine_resources.py bin/resources

export MSIME_TEST_DATABASE_URL='postgres://postgres:local-test@127.0.0.1:5432/msime_auth_test?sslmode=disable'
export MSIME_ENGINE_TEST_BINARY="$PWD/bin/native/msime-engine"
export MSIME_ENGINE_TEST_RESOURCES="$PWD/bin/resources"
go test -race -count=1 -p 1 -json -coverpkg=./... -coverprofile=bin/api.cover ./... > bin/api-tests.jsonl
python3 scripts/check_api_test_results.py bin/api-tests.jsonl
python3 scripts/check_go_coverage.py bin/api.cover
go tool cover -func=bin/api.cover
```

`-count=1` 不能省：`-coverpkg=./...` 下，命中测试缓存的包会重放旧的覆盖率文件，其中也列出它没有导入、之后改过的包，块位置还是旧源码，合并后多出一批为 0 的旧块，算出的覆盖率偏低。

`TestAPICoverageInventory` 检查 OpenAPI 操作没有漏登记，且登记的业务测试函数真实存在。`check_api_test_results.py` 进一步读取 Go 测试事件，要求登记用例实际为 `pass`；缺失、跳过、失败均不通过。CI 的原生引擎任务执行这两层检查。单独执行不带数据库或引擎环境变量的 `go test ./...` 仍适合快速检查，但不代表完整 API 回归。

真实 Apple/Windows 客户端联调和实际供应商流式调用仍是显式启用的附加测试，不作为后端 API 回归前提；其协议与成功/失败响应已由本地可重复运行的服务测试覆盖。语句覆盖率用于定位盲区，不宣称所有函数达到 100%。

十万词条容量验证需显式开启，可在上述环境配置完成后运行：

```sh
MSIME_TEST_LARGE_RESTORE=1 go test -race ./internal/account -run '^TestSnapshotRestoreAtEntryLimit$' -count=1
```

## 语句覆盖率门禁

`check_go_coverage.py` 要求全仓库、`internal/account`、`internal/server` 的语句覆盖率分别达到 **90%**。统计包括 CLI 入口、Go 内嵌前端和所有有可执行语句的包，不排除低覆盖文件。`-coverpkg=./...` 计入跨包调用，并按代码块合并重复记录；CI 以未四舍五入的覆盖数判断门槛。`-p 1` 让包测试依次运行，减少原生引擎与编译任务的资源竞争。

数据库故障测试使用独立测试库，在 pgx 查询入口逐条取消 SQL，检查事务前后的词库、变更流、剪贴板、偏好、会话、社区资源与审计数据一致。原生编辑、排序、恢复与导入仍使用真实引擎。公开 HTTP 路由的成功和鉴权测试继续经过路由器；事务故障测试直接调用处理器，以免 IP 限流的数据库写入提前遮蔽待验证的失败分支。

门禁脚本本身也有测试，覆盖代码块去重、临界值、缺失包、畸形数据和不足覆盖率：

```sh
python3 -m unittest discover -s scripts -p 'test_go_coverage.py'
```

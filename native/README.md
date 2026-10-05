# 公共引擎进程

后端以子进程调用 msime 主仓库的 Rust 引擎：`third_party/msime` 子模块里的 `msime-backend-engine`，协议实现在 `crates/engine/src/backend`。单次 JSON 请求最长 64 KiB、响应最长 1 MiB、执行最长 10 秒。每次查询使用独立临时目录，完成或失败后由 Go 清理，不保存学习记录。请求不能指定程序、资源路径或执行参数。

引擎与词库都随子模块的 commit 固定：程序从子模块编译，词库按子模块里桌面客户端的锁文件 `resources/desktop-dictionary.lock.json` 下载，辅助码表取自子模块的 `resources/helpcodes`。所以服务端查重、候选和注音用的词库与客户端发布的完全一致，升级时移动子模块即可，两者一起变。此前桥接的是已归档的 C++ MSIME-Engine 及其 dict-v2.0.1 词库，与客户端各走各的，投稿查重因此漏掉了新词库已收的词。

## 构建与验证

依赖：Rust 工具链（版本由子模块的 `rust-toolchain.toml` 固定，rustup 会自动安装）。

```sh
git submodule update --init
(cd third_party/msime && cargo build -p msime-backend-engine --release --locked)
python3 scripts/fetch_engine_resources.py bin/resources
MSIME_ENGINE_TEST_BINARY="$PWD/third_party/msime/target/release/msime-backend-engine" python3 scripts/test_native.py
MSIME_ENGINE_TEST_BINARY="$PWD/third_party/msime/target/release/msime-backend-engine" MSIME_ENGINE_TEST_RESOURCES="$PWD/bin/resources" go test -race ./internal/server
python3 scripts/fetch_engine_resources.py bin/resources
```

下载脚本按锁文件验证每个发布文件的长度、SHA-256 和词库清单里的来源 commit。查询后的再次运行用于确认基础资源未被修改。发现不匹配时脚本拒绝覆盖，更新资源应在新目录校验后切换配置。

服务配置示例（路径需绝对路径）：

```json
{
  "engine": {
    "binary": "/usr/local/bin/msime-engine",
    "resources": "/data/resources"
  }
}
```

这是完整配置中的 `engine` 字段，鉴权和其他配置仍按后端配置提供。生产镜像把 `msime-backend-engine` 装为 `/usr/local/bin/msime-engine`，内置已校验资源，资源目录为 `/usr/share/msime`；自定义资源目录需挂载并校验；生产 `docs_enabled` 保持 `false`。服务账号仅需基础资源的读取权限和临时目录写入权限。

HTTP 查询与返回结构由 `scripts/generate_openapi.py` 生成，开发环境显式启用文档后可查看。新能力清单由 `GET /v1/input/capabilities` 提供。

引擎进程覆盖无状态查询、词条校验、简繁转换（OpenCC s2t 数据，内置于程序）和词组注音（取词库里该词的读音，没有时逐字取最常用读音）。四类用户词库 CRUD、事务导入导出、纯汉字导入与增量变更记录有真实 PostgreSQL + 引擎测试；个人候选查询会读取同一 PostgreSQL 快照中的最终覆盖和版本，由引擎回放至临时词库副本，支持全拼、双拼、五笔、英文、快捷短语和简拼。空覆盖直接查询基础词库；非空覆盖只修改副本。用户词库日志的表结构与此前的 C++ 引擎相同，已存的覆盖无需迁移。调频、固定位置、完整快照导出与原子恢复的验收见 [公共 API 清单](../docs/windows-api-extraction.md)。

个人查询入口为 `POST /v1/users/me/dictionary/candidates`，仅接受用户会话。请求字段包括 `kind`、`text`、`scheme`、`profile`、`limit`，返回候选与 `revision`。数据库保存最终覆盖及完整变更日志，同一词条的反复更新不增加查询回放条数；迁移可从日志重建最终覆盖。所有原生请求最多同时执行 4 个，临时词库空间按并发副本预留，查询完成、取消或失败后均由宿主清理。

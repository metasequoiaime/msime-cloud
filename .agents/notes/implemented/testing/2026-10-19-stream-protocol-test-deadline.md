# Agent Note: 流式协议边界测试使用有余量的功能期限

Status: implemented

## Problem

Ubuntu CI 的 `TestStreamingProtocolViolationsMeterTheirSource/上游/会话超限` 在 20.02 秒返回 `context deadline exceeded`，没有读到预期的关闭状态。测试必须经中继的两段 TLS 转发真实的 32 MiB 会话边界数据；相同代码在 macOS 的全仓与数据库 race 测试中通过，原来的 20 秒同时充当了未声明的吞吐要求。改为一分钟后，普通 Ubuntu、Windows race 任务通过，但 PostgreSQL 的 `-coverpkg=./internal/account,./internal/server` 联合覆盖率任务在 60.03 秒只转发 28,311,552 字节，仍未抵达边界；验证必须覆盖同样的插桩组合。

## Decision

测试双方共享五分钟的功能期限，并沿用主分支每条消息独立的 20 秒读写等待上限；服务端 fixture 的会话期限使用允许上限 600 秒，避免会话正常结束先于协议违规处理。生产的期限与 32 MiB 契约均保持原值；仍断言完整合法边界字节数、违规消息不转发、准确的 WebSocket 关闭状态和分钟／小时计量。失败信息附带已转发字节数，便于区分慢传输和关闭握手问题。

本次检索活跃笔记的流式、协议违规和 streaming 机制，没有已有归属，因此单列测试策略记录；与候选皮肤资源协议的功能决定独立。

## Alternatives considered

保留 20 秒可以更快终止挂起，但这项测试不承担吞吐门禁，共享 runner 与 race 的加密开销会让正确实现被偶然拒绝，因此采用仍有界且只在失败时变长的五分钟期限。

## Testing

远端 `20261019-193756` 用 `GOMAXPROCS=1 go test -race -count=10 -timeout=5m -run "^TestStreamingProtocolViolationsMeterTheirSource$" ./internal/server` 连续执行十轮、共四十个分支，54.004 秒通过。该命令未带联合覆盖率，不能代替数据库 CI 的同类复跑。补充 `GOMAXPROCS=1 go test -race -coverpkg=./internal/account,./internal/server -count=3 -timeout=10m -run "^TestStreamingProtocolViolationsMeterTheirSource$" ./internal/server`，十二个分支在 22.914 秒通过；vet、契约与 OpenAPI 校验通过。跨平台复跑由 PR 的 Ubuntu、Windows 与数据库 CI 覆盖。

## Consequences

正常测试耗时不变，失败时最多等待五分钟；真实消息上限和全部行为断言仍接受同一套验证。

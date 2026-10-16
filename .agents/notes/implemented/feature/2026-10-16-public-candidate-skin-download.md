# Agent Note: 公开候选窗皮肤允许匿名下载

Status: implemented

## Problem

官网已经公开展示候选窗皮肤，但下载接口强制要求用户会话，访客无法直接取得公开素材。下载不应成为注册或登录的门槛。

## Decision

现有 POST `/v1/community/candidate-skins/{id}/download` 接受匿名请求：没有有效凭据时只允许下载公开且未下架的作品；私有或已下架作品仍只对拥有者开放。不存在或不允许访问统一返回 404，不返回包内容。认证查询失败、取消或超时必须返回错误，不能当成匿名请求继续执行；因此下载接口单独区分认证错误，不能复用吞掉所有认证错误的浏览辅助函数 `communityViewer`。

匿名下载不创建账号、下载行或新统计表。有效账号继续使用用户事务、封禁复查和下载唯一键，保留按账号去重的人数定义。两种请求都在事务中锁定皮肤行，防止可见性修改或删除与下载读取交错；来源限制、数据库按地址限流和返回包格式保持不变。

## Alternatives considered

新增匿名 GET 下载接口可以单独表达公开能力，但会增加路由与客户端分流，现有 POST 本身能够按可见性安全读取，所以直接放宽现有接口。匿名下载逐次累加更接近文件下载次数，但会改变现有「下载人数」定义，还需要处理重试与刷量；因此保留账号去重，匿名下载不计数。

## Consequences

官网未登录访客可直接保存 ZIP，旧客户端的已登录下载不变，无需数据库迁移。统计只表示有会话的下载账号数，不代表全部文件下载次数。网站依赖本接口放宽权限后再部署。

## Verification

PostgreSQL 集成用例核对匿名、格式无效、格式有效但不存在的令牌与有效会话取得相同包，覆盖私有、下架、不存在、拥有者权限和下载去重。`TestUserDataTransactionsRollbackAtEveryDatabaseStatement/download_candidate_skin` 在每一条认证和下载 SQL 上注入取消，验证失败能被识别且没有残留写入；原实现漏掉认证取消，完整 PostgreSQL CI 会失败。完整检查结果随 PR 记录。

## Related notes audit

仓库没有既有 `.agents/notes/`、ADR 或决策目录；接口约束来源是 `docs/skin-community.md` 和 OpenAPI 生成器，本次同时更新。共享 `contracts/protocol.json` 不包含社区皮肤操作，无需修改。

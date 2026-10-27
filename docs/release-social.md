# 发版社交分发

后端版本与镜像发布工作流在镜像构建、GitHub Release 创建以及 `msime-cloud` 命令行资产上传完成后，执行最后的 `announce` job。这个 job 不直接保存各个平台的账号令牌，只把完整 Release 封装成一份带 HMAC 签名的 JSON，发送到社交分发 webhook。

## 配置 GitHub Secrets

在仓库的 Settings → Secrets and variables → Actions 中新增：

| Secret | 内容 |
| --- | --- |
| `SOCIAL_WEBHOOK_URL` | n8n、Make 或自建分发服务的 HTTPS webhook 地址 |
| `SOCIAL_WEBHOOK_SECRET` | 与分发服务约定的随机共享密钥 |

两个 Secret 都配置后，下一次正式 Release 会自动发送。只配置一个时 workflow 会跳过分发，不会影响镜像或 Release 发布。密钥只在 Actions 环境变量中使用，不要写进仓库、Release 说明或日志。

## Webhook 合约

请求方法是 `POST`，正文是 UTF-8 JSON，带有以下请求头：

- `X-MSIME-Signature: sha256=<HMAC-SHA256>`：对原始请求正文用 `SOCIAL_WEBHOOK_SECRET` 计算。
- `X-MSIME-Idempotency-Key: metasequoiaime/msime-cloud:backend-v1.2.3`：下游必须按此键去重。
- `Content-Type: application/json`。

正文包含 `schema_version`、`event`、`idempotency_key`、`repository` 和 `release`。`release` 中有 `tag`、`name`、`body`、`url`、`published_at` 和 `target_commitish`。下游应先验签和去重，再按平台限制生成短帖、长帖或线程。

分发服务建议把 Telegram、Discord、Mastodon 和 Bluesky 设为自动发布，把 X、微博、小红书和 Reddit 设为人工审核或延迟发布。每个平台单独记录发布结果，失败时只重试网络错误和 5xx；不要把上游响应正文原样转发到日志或帖子。

## 手动补发

在 Actions 页面重新运行同一个发版 workflow 会复用已有标签并重新构建缺失的 Release 资产。社交分发以版本标签为幂等键，下游应把已经成功的 `backend-v1.2.3` 判为已发送；需要重新发布时，应在分发服务中明确执行补发，而不是直接删除去重记录。

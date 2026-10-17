# Agent Note: 记录每日邮件投递量

Status: implemented

## Problem

验证码和下载链接共用每日邮件额度，但后台没有历史数据可回答每天实际调用了多少次邮件供应商、多少次被接受或拒收。限流表只保存当前窗口，且失败退款后不能作为历史统计。

## Decision

服务在真正调用邮件供应商后，按 UTC 日期将验证码和下载链接的结果写入 PostgreSQL 的 `admin_email_delivery_daily`。每行保存 `attempted`、`accepted`、`rejected` 和 `failed` 四个互斥结果计数；进入 SMTP `DATA` 后即使确认阶段报错也计入 `accepted`，RCPT 阶段永久拒收计入 `rejected`，其他未交给供应商的错误计入 `failed`。统计写入使用独立的短期限请求，失败只记录日志，不影响邮件响应。管理后台总览返回最近请求范围内的每日行并显示这四类计数，过期数据由定时清理保留 180 天。

## Alternatives considered

- 直接展示 `auth_rates` 中的 `delivery-global`：它只代表当前额度窗口，无法回看历史，也会把退款和真实供应商结果混在一起。
- 只在预留额度时计数：预留可能随后被连接、认证或 RCPT 失败退回，不能代表实际发信尝试。
- 只保存一个总数：无法区分地址永久拒收与供应商故障，后台无法判断 450 次额度被什么消耗。

## Testing

- 单元测试覆盖成功、永久拒收、进入 `DATA` 后失败和普通失败的分类。
- PostgreSQL 总览测试覆盖按 UTC 日期补齐空行并返回每日投递字段；迁移清单覆盖新表的启动迁移。
- 前端 lint/build 和全仓 Go 验证检查确认 API 类型、后台构建产物和服务代码一致。

## Consequences

后台可以长期观察每日实际邮件调用和失败构成，多副本写入共享数据库而不会丢统计。统计表增加一次按邮件调用的 PostgreSQL upsert，保留 180 天并在 `Store.Prune` 中清理；统计表或数据库短暂不可用时，邮件请求仍可完成，但该次统计会缺失。

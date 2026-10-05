# 诊断快照与远程 MCP

安卓开发者选项里的「上传诊断日志」把用户勾选的几类诊断信息上传为一份快照，再给出一个远程 MCP 地址和访问令牌。用户把这两样发给开发者，开发者在自己的 MCP 客户端（例如 Claude Code）里只读地查看这份快照。服务端只保存用户确认上传的内容，到期自动删除。

## 客户端接口

都在 `/v1/users/me/diagnostics` 下，需要用户会话（匿名账号也可以），设备令牌返回 401。请求和响应的完整 schema 见 OpenAPI 的「诊断」标签。

- `POST /v1/users/me/diagnostics`：上传快照。JSON 最多 2 MiB：`{"platform","app_version","sections":{...},"ttl"}`，`ttl` 是 `one_hour`、`one_day` 或 `seven_days`。`sections` 里只出现用户勾选的分类，至少一个：
  - `crash_logs`：`[{"at","message","stack"}]`，`at` 是 RFC 3339 时间，`message` 最多 2 KiB，`stack` 最多 16 KiB，最多 200 条。
  - `perf_trace`：`[{"t_ms","kind","duration_ms"}]`，三个字段都必须有。
  - `input_events`：`[{"t_ms","kind","duration_ms"?}]`，`duration_ms` 可以省略。
  - `config_snapshot`：设置快照对象，最多 8 层、2000 个条目，字符串最多 1 KiB。键名匹配 `(?i)token|secret|password|api_key|key$` 的值必须是字符串 `"<redacted>"`。

  `perf_trace` 与 `input_events` 的 `kind` 只能是 `key_down`、`key_up`、`candidate_shown`、`candidate_selected`、`commit`、`backspace`、`panel_open`、`panel_close`、`ime_start`、`ime_finish`。事件对象只允许列出的字段，任何多出来的字段（文本、拼音、候选、按键字符）、未知的分类、超长字符串或未脱敏的凭据都让整份上传返回 400，不会只存一部分。错误码：`invalid_json`、`invalid_client`、`invalid_ttl`、`empty_snapshot`、`invalid_crash_logs`、`invalid_perf_trace`、`invalid_input_events`、`invalid_config_snapshot`、`unredacted_config`。

  成功返回 201 `{"id","mcp_url","token","expires_at"}`，`mcp_url` 是 `https://api.msime.app/mcp/s/<id>`。令牌是 `msk_` 加 43 位 base64url，只在这个响应里出现一次，库里只存 SHA-256 和最后 4 个字符。每个用户只有一份有效快照：新上传在同一事务里删除旧快照和它的访问记录。每个用户每小时 6 次（`auth_rates`，所有副本共享），校验失败的请求不计数。
- `GET /v1/users/me/diagnostics`：`{"snapshot":{"id","created_at","expires_at","bytes","sections","token_hint"}|null,"accesses":[{"at","tool","arguments","result_count","bytes"}]}`。不返回快照内容和令牌；`accesses` 是最近 50 次工具调用，新的在前，`arguments` 超过 1 KiB 时截断成字符串。
- `POST /v1/users/me/diagnostics/token`：换一个新令牌，旧令牌立即失效，返回 200 `{"token"}`；没有有效快照返回 404 `diagnostics_not_found`。每个用户每小时 20 次。
- `DELETE /v1/users/me/diagnostics`：删除快照和访问记录，返回 204（没有快照也是 204）。

## MCP 端点

`POST /mcp/s/{id}`（以及 `GET`）是 MCP Streamable HTTP 端点，用官方 Go SDK `github.com/modelcontextprotocol/go-sdk` 的 `mcp` 包实现，不手写协议。它不在 `/v1` 下，`account.IsPath` 让它绕过全局 Bearer 检查：

- 鉴权：`Authorization: Bearer msk_…`，服务端按 SHA-256 常量时间比较。ID 不存在、已过期、令牌不对、账号被封禁都返回同样的 401，不区分原因。用户会话和设备令牌都不能读快照。
- 限流：每份快照每分钟 60 次（`auth_rates`，所有副本共享），令牌错误的请求也计数，超出返回 429 并带 `Retry-After: 60`。
- 不要求 `Origin`；带了不在 `allowed_origins` 里的 `Origin`（浏览器跨站请求）照样返回 403。
- 无状态模式：生产多副本、没有粘性会话，所以每个请求单独建一个只读 MCP server，不发 `Mcp-Session-Id`，响应是 `application/json`；`GET` 在无状态模式下返回 405，客户端不需要服务端推送。

工具全部只读，按快照包含的分类出现，没有上传的分类没有对应工具：

| 工具 | 参数 | 返回 |
| --- | --- | --- |
| `read_crash_logs` | `limit?` | `{"crash_logs":[{"at","message","stack"}]}` |
| `get_perf_trace` | `kind?`、`limit?` | `{"events":[{"t_ms","kind","duration_ms"}]}` |
| `get_config_snapshot` | 无 | `{"config":{...}}`，凭据类的值是 `"<redacted>"` |
| `get_input_events` | `kind?`、`limit?` | `{"events":[{"t_ms","kind","duration_ms"?}]}`，只在用户勾选了输入事件时出现 |

事件类工具按白名单字段重新构造输出，只有 `t_ms`、`kind`、`duration_ms`。没有任何能读取输入文本的工具。每次工具调用写一条访问记录（工具名、参数、返回条数、结果的 JSON 字节数），记录写不进去时调用返回错误而不是静默放行，保证用户在 App 里看到每一次读取。

## 隐私要点

- 默认关闭，每次上传都由用户在开发者选项里确认；只上传用户勾选的分类，「输入事件」默认不勾选并单独说明。
- 输入事件只有时间戳、种类和耗时，永远不含文本、拼音、候选或按键字符。客户端只能从枚举构造事件，服务端再校验一次，不合规的整份拒绝。
- 设置快照里的凭据在客户端脱敏，服务端拒绝任何未脱敏的凭据类键。
- 拿到地址和令牌的人都能读这份快照，所以界面提醒只发给开发者；用户可以随时重新生成令牌或删除快照。
- 快照按 `ttl` 到期，由每小时的清理任务（`Store.Prune`）删除，访问记录随外键一起删除；注销账号时级联删除。
- 服务端日志不记录快照内容、工具参数或令牌。

## 存储

- `diagnostic_snapshots(id, user_id → auth_users ON DELETE CASCADE, token_hash UNIQUE, token_hint, platform, app_version, sections text[], content jsonb, bytes, created_at, expires_at)`，`user_id` 上有唯一索引，所以每个用户最多一行；过期但还没被清理的行在下一次上传时一起删掉。
- `diagnostic_accesses(id bigserial, snapshot_id → diagnostic_snapshots ON DELETE CASCADE, tool, arguments jsonb, result_count, bytes, at)`。

按最小权限部署时，运行角色需要这两张表的 `SELECT, INSERT, UPDATE, DELETE` 和序列 `diagnostic_accesses_id_seq` 的 `USAGE, SELECT`，见 README「多副本部署」。

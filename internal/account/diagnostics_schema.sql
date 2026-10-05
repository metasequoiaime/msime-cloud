-- 用户在开发者选项里确认上传的诊断快照：只含用户勾选的分类，经远程 MCP 端点 /mcp/s/{id} 只读访问。令牌只存 SHA-256，到期由 Store.Prune 删除，注销账号时级联删除。每个用户最多一份：新上传在同一事务里先删旧的。
CREATE TABLE IF NOT EXISTS diagnostic_snapshots (
 id text PRIMARY KEY,
 user_id text NOT NULL REFERENCES auth_users(id) ON DELETE CASCADE,
 token_hash text NOT NULL UNIQUE,
 token_hint text NOT NULL,
 platform text NOT NULL,
 app_version text NOT NULL,
 sections text[] NOT NULL,
 content jsonb NOT NULL,
 bytes integer NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 expires_at timestamptz NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS diagnostic_snapshots_user ON diagnostic_snapshots(user_id);
CREATE INDEX IF NOT EXISTS diagnostic_snapshots_expires ON diagnostic_snapshots(expires_at);
-- 每次 MCP 工具调用一条，随快照一起删除；arguments 截断到 1 KiB，不含快照内容。
CREATE TABLE IF NOT EXISTS diagnostic_accesses (
 id bigserial PRIMARY KEY,
 snapshot_id text NOT NULL REFERENCES diagnostic_snapshots(id) ON DELETE CASCADE,
 tool text NOT NULL,
 arguments jsonb NOT NULL,
 result_count integer NOT NULL,
 bytes integer NOT NULL,
 at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS diagnostic_accesses_snapshot ON diagnostic_accesses(snapshot_id, at DESC);

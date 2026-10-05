-- 用户在 App 里提交的反馈。只给管理后台看，不公开；保留 180 天，由 Store.Prune 清理，注销账号时级联删除。
CREATE TABLE IF NOT EXISTS feedback (
 id text PRIMARY KEY,
 user_id text NOT NULL REFERENCES auth_users(id) ON DELETE CASCADE,
 type text NOT NULL CHECK(type IN ('bug','suggestion','dictionary')),
 text text NOT NULL,
 platform text NOT NULL,
 app_version text NOT NULL,
 edition text NOT NULL DEFAULT '',
 diagnostics jsonb NOT NULL DEFAULT '{}',
 status text NOT NULL DEFAULT 'new' CHECK(status IN ('new','resolved')),
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS feedback_created ON feedback(created_at);
CREATE INDEX IF NOT EXISTS feedback_user ON feedback(user_id);
-- 截图经服务端重新编码（去掉 EXIF 等元数据）后存在库里，不进公开的头像存储。
CREATE TABLE IF NOT EXISTS feedback_screenshots (
 feedback_id text NOT NULL REFERENCES feedback(id) ON DELETE CASCADE,
 position smallint NOT NULL CHECK(position BETWEEN 0 AND 2),
 mime text NOT NULL CHECK(mime IN ('image/png','image/jpeg')),
 bytes bytea NOT NULL,
 PRIMARY KEY(feedback_id,position)
);

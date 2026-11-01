-- 输入习惯同步（learning 域）的端到端加密存储，协议见 msime 主仓库的 docs/learning-sync.md。服务端只保存设备公钥、密钥编号、封装和不透明的密文信封，读不到内容。四张表都和云剪贴板的分开：关闭云剪贴板或删除剪贴板数据不能让输入习惯失效，反过来也一样。
-- 账号当前的 learning 密钥编号 `key_id`（小写十六进制 32 位）。只在有密钥时有行；删除最后一台设备时一并删除。
CREATE TABLE IF NOT EXISTS user_learning_settings (
 user_id text PRIMARY KEY REFERENCES auth_users(id) ON DELETE CASCADE,
 key_id text NOT NULL,
 updated_at timestamptz NOT NULL DEFAULT now()
);
-- 登记过输入习惯同步的设备：`device_id` 是公钥 SHA-256 的前 16 字节（小写十六进制），`public_key` 是 X25519 公钥的 base64url。每台设备绑定登记它的会话，一个会话只绑定一台设备；会话过期删除时随外键删除，会话被吊销时由 `user_learning_session_revoked` 触发器删除。封装和密文都随设备的外键删除。
CREATE TABLE IF NOT EXISTS user_learning_devices (
 user_id text NOT NULL REFERENCES auth_users(id) ON DELETE CASCADE,
 device_id text NOT NULL,
 public_key text NOT NULL,
 name text NOT NULL,
 session_id text NOT NULL REFERENCES auth_sessions(id) ON DELETE CASCADE,
 created_at timestamptz NOT NULL DEFAULT now(),
 last_seen timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(user_id,device_id),
 CONSTRAINT user_learning_devices_session UNIQUE(session_id)
);
-- 长期不在线的设备由 `Store.Prune` 按最后在线时间清理。
CREATE INDEX IF NOT EXISTS user_learning_devices_last_seen ON user_learning_devices(last_seen);
-- 当前密钥给每台已批准设备的封装，以及封装它的批准者。换钥时全部删除后重写，所以每台设备最多一行。批准者不设外键：批准者之后关闭功能、删掉自己时，已经批准的设备不能因此失去封装。
CREATE TABLE IF NOT EXISTS user_learning_wraps (
 user_id text NOT NULL,
 device_id text NOT NULL,
 key_id text NOT NULL,
 wrap text NOT NULL,
 approver_device_id text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(user_id,device_id),
 FOREIGN KEY(user_id,device_id) REFERENCES user_learning_devices(user_id,device_id) ON DELETE CASCADE
);
-- 每台设备一份密文信封（MSL1），`revision` 是信封头里的代次，`bytes` 是信封字节数。信封最多 8 MiB，代次在 1 到 2^53-1 之间。
CREATE TABLE IF NOT EXISTS user_learning_blobs (
 user_id text NOT NULL,
 device_id text NOT NULL,
 key_id text NOT NULL,
 revision bigint NOT NULL CONSTRAINT user_learning_blobs_revision_check CHECK(revision BETWEEN 1 AND 9007199254740991),
 blob bytea NOT NULL CONSTRAINT user_learning_blobs_size_check CHECK(octet_length(blob) BETWEEN 84 AND 8388608),
 bytes integer GENERATED ALWAYS AS (octet_length(blob)) STORED,
 updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(user_id,device_id),
 FOREIGN KEY(user_id,device_id) REFERENCES user_learning_devices(user_id,device_id) ON DELETE CASCADE
);
-- 密文压不动，按 EXTERNAL 存放，跳过 TOAST 的压缩尝试。可以重复执行。
ALTER TABLE user_learning_blobs ALTER COLUMN blob SET STORAGE EXTERNAL;
-- 会话被吊销（退出登录、撤销设备、封禁、刷新令牌重放）时删除绑定它的设备，封装和密文随外键删除；账号因此没有设备时一并删除密钥编号。旧版本副本的吊销也经过这个触发器。使用触发表所在的 schema，避免调用者的 search_path 把删除导向同名表。
CREATE OR REPLACE FUNCTION user_learning_session_revoked() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 EXECUTE format('DELETE FROM %I.user_learning_devices WHERE session_id=$1',TG_TABLE_SCHEMA) USING NEW.id;
 EXECUTE format('DELETE FROM %I.user_learning_settings s WHERE s.user_id=$1 AND NOT EXISTS(SELECT 1 FROM %I.user_learning_devices d WHERE d.user_id=$1)',TG_TABLE_SCHEMA,TG_TABLE_SCHEMA) USING NEW.user_id;
 RETURN NULL;
END $$;
DO $$ BEGIN
 IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgrelid='auth_sessions'::regclass AND tgname='user_learning_session_revoked') THEN
  CREATE TRIGGER user_learning_session_revoked AFTER UPDATE OF revoked ON auth_sessions FOR EACH ROW WHEN (NEW.revoked AND NOT OLD.revoked) EXECUTE FUNCTION user_learning_session_revoked();
 END IF;
END $$;
ALTER TABLE auth_sessions ENABLE TRIGGER user_learning_session_revoked;

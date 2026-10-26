-- 用户发布的插件包（zip 内含 plugin.toml、被引用的音频或数据文件以及说明文本）。id 是客户端生成的发布 UUID；plugin_id 是清单里的 id，也是客户端的安装目录名。归档在校验后按上传原样保存，服务端从不解码或执行；size 和 sha256 是生成列，因此不会与下载返回的字节不一致。
CREATE TABLE IF NOT EXISTS community_plugins (
 id text PRIMARY KEY CHECK(id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
 owner_id text NOT NULL REFERENCES auth_users(id) ON DELETE CASCADE,
 kind text NOT NULL CONSTRAINT community_plugins_kind_known CHECK(kind IN ('sound','music','command_table','effect','helpcode','symbol_set','phrase_table','wordbook','script')),
 plugin_id text NOT NULL CHECK(plugin_id ~ '^[a-z0-9][a-z0-9._-]{0,63}$'),
 name text NOT NULL,
 description text NOT NULL DEFAULT '',
 version text NOT NULL CHECK(length(version) BETWEEN 1 AND 32),
 license text NOT NULL CHECK(length(license) BETWEEN 1 AND 64),
 manifest bytea NOT NULL CHECK(octet_length(manifest) BETWEEN 1 AND 262144),
 archive bytea NOT NULL CHECK(octet_length(archive) BETWEEN 1 AND 8388608),
 size integer GENERATED ALWAYS AS (octet_length(archive)) STORED,
 sha256 text GENERATED ALWAYS AS (encode(sha256(archive),'hex')) STORED,
 -- sha256 over the canonical upload (name, description, kind, plugin_id, version, then the archive digest), so an identical retry is recognised.
 request_sha256 text NOT NULL CHECK(request_sha256 ~ '^[0-9a-f]{64}$'),
 created_at timestamptz NOT NULL DEFAULT now()
);
-- 特效包之前建的表带列上自动命名的 kind 约束，不含 'effect'；先删掉它，再由下面换成命名约束。
ALTER TABLE community_plugins DROP CONSTRAINT IF EXISTS community_plugins_kind_check;
-- 命名约束不含 'script'（还没有脚本插件，或更早还没有辅助码表、符号集、短语表和单词本）时整体替换。约束一次列出客户端认识的全部类型，某个类型能否发布由服务端的 pluginKinds 决定；以后再加类型时把这里探测的类型名换成新加的那个，并同步 store.go 的 consoleReady。
DO $$
BEGIN
 IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid='community_plugins'::regclass AND conname='community_plugins_kind_known' AND pg_get_constraintdef(oid) LIKE '%''script''%') THEN
  ALTER TABLE community_plugins DROP CONSTRAINT IF EXISTS community_plugins_kind_known;
  ALTER TABLE community_plugins ADD CONSTRAINT community_plugins_kind_known CHECK(kind IN ('sound','music','command_table','effect','helpcode','symbol_set','phrase_table','wordbook','script'));
 END IF;
END $$;
-- 脚本插件申请的能力名（清单 permissions，按清单顺序），给管理后台的审核列表看；数据类插件为空数组。旧版本副本插入时不写它，取默认值。
ALTER TABLE community_plugins ADD COLUMN IF NOT EXISTS permissions text[] NOT NULL DEFAULT '{}';
CREATE INDEX IF NOT EXISTS community_plugins_newest ON community_plugins(created_at DESC,id);
CREATE INDEX IF NOT EXISTS community_plugins_kind_newest ON community_plugins(kind,created_at DESC,id);
CREATE INDEX IF NOT EXISTS community_plugins_owner ON community_plugins(owner_id,created_at DESC);
CREATE TABLE IF NOT EXISTS community_plugin_downloads (
 pack_id text NOT NULL REFERENCES community_plugins(id) ON DELETE CASCADE,
 user_id text NOT NULL REFERENCES auth_users(id) ON DELETE CASCADE,
 PRIMARY KEY(pack_id,user_id)
);
-- 账号注销按 user_id 触发外键级联；主键以 pack_id 开头，不能避免扫描整张互动表。
CREATE INDEX IF NOT EXISTS community_plugin_downloads_user ON community_plugin_downloads(user_id);
CREATE TABLE IF NOT EXISTS community_plugin_ratings (
 pack_id text NOT NULL REFERENCES community_plugins(id) ON DELETE CASCADE,
 user_id text NOT NULL REFERENCES auth_users(id) ON DELETE CASCADE,
 stars integer NOT NULL CHECK(stars BETWEEN 1 AND 5),
 PRIMARY KEY(pack_id,user_id)
);
CREATE INDEX IF NOT EXISTS community_plugin_ratings_user ON community_plugin_ratings(user_id);
-- 插件的收藏，与 community_skin_saves 相同：created_at 是收藏时间，`scope=saved` 按它倒序列出。
CREATE TABLE IF NOT EXISTS community_plugin_saves (
 pack_id text NOT NULL REFERENCES community_plugins(id) ON DELETE CASCADE,
 user_id text NOT NULL REFERENCES auth_users(id) ON DELETE CASCADE,
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(pack_id,user_id)
);
CREATE INDEX IF NOT EXISTS community_plugin_saves_user ON community_plugin_saves(user_id,created_at DESC);

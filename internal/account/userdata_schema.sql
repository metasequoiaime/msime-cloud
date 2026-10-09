-- 用户同步数据与鉴权用户共用生命周期，删除账号时级联清理。
CREATE TABLE IF NOT EXISTS user_preferences (
 user_id text PRIMARY KEY REFERENCES auth_users(id) ON DELETE CASCADE,
 revision bigint NOT NULL DEFAULT 0,
 settings jsonb NOT NULL DEFAULT '{}'
);
CREATE TABLE IF NOT EXISTS user_clipboard_settings (
 user_id text PRIMARY KEY REFERENCES auth_users(id) ON DELETE CASCADE,
 enabled boolean NOT NULL DEFAULT false
);
CREATE TABLE IF NOT EXISTS user_clipboard (
 id text PRIMARY KEY,
 user_id text NOT NULL REFERENCES auth_users(id) ON DELETE CASCADE,
 text text NOT NULL,
 text_hash text NOT NULL,
 sequence bigint NOT NULL,
 updated_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(user_id,text_hash)
);
CREATE INDEX IF NOT EXISTS user_clipboard_order ON user_clipboard(user_id,sequence DESC);
CREATE TABLE IF NOT EXISTS user_dictionary_state (
 user_id text PRIMARY KEY REFERENCES auth_users(id) ON DELETE CASCADE,
 revision bigint NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS user_dictionary_entries (
 id text PRIMARY KEY,
 user_id text NOT NULL REFERENCES auth_users(id) ON DELETE CASCADE,
 kind text NOT NULL CONSTRAINT user_dictionary_entries_kind_check CHECK(kind IN ('pinyin','wubi','wubi98','english','quick')),
 code text NOT NULL,
 word text NOT NULL,
 weight bigint NOT NULL CHECK(weight>=0),
 revision bigint NOT NULL,
 updated_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(user_id,kind,code,word)
);
CREATE INDEX IF NOT EXISTS user_dictionary_entries_order ON user_dictionary_entries(user_id,kind,code,word);
-- 词条种类加入 98 版五笔 wubi98（安卓导出的快照里 98 版词条的 kind）。旧库的列约束（PostgreSQL 命名为 user_dictionary_entries_kind_check）不含它时整体替换，可以重复执行。新约束只放宽取值，已有行都满足旧约束也就满足新约束，所以以 NOT VALID 添加：不回头扫描整张表，迁移持有的排他锁只到替换完成为止，不改写任何数据。
DO $$
BEGIN
 IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid='user_dictionary_entries'::regclass AND conname='user_dictionary_entries_kind_check' AND pg_get_constraintdef(oid) LIKE '%wubi98%') THEN
  ALTER TABLE user_dictionary_entries DROP CONSTRAINT IF EXISTS user_dictionary_entries_kind_check;
  ALTER TABLE user_dictionary_entries ADD CONSTRAINT user_dictionary_entries_kind_check CHECK(kind IN ('pinyin','wubi','wubi98','english','quick')) NOT VALID;
 END IF;
END $$;
CREATE TABLE IF NOT EXISTS user_dictionary_changes (
 user_id text NOT NULL REFERENCES auth_users(id) ON DELETE CASCADE,
 revision bigint NOT NULL,
 change jsonb NOT NULL,
 PRIMARY KEY(user_id,revision)
);
-- 最终用户覆盖是变更日志的事务投影；反复修改同一词条不会增加查询回放量。
CREATE TABLE IF NOT EXISTS user_dictionary_overlay (
 user_id text NOT NULL REFERENCES auth_users(id) ON DELETE CASCADE,
 kind text NOT NULL,
 code text NOT NULL,
 word text NOT NULL,
 entry jsonb NOT NULL,
 deleted boolean NOT NULL,
 PRIMARY KEY(user_id,kind,code,word)
);
WITH latest_resets AS MATERIALIZED (
 SELECT user_id,max(revision) AS reset_revision FROM user_dictionary_changes
 WHERE change->>'reset'='true' GROUP BY user_id
)
INSERT INTO user_dictionary_overlay(user_id,kind,code,word,entry,deleted)
SELECT DISTINCT ON(user_id,item->>'kind',item->>'code',item->>'word')
 user_id,item->>'kind',item->>'code',item->>'word',item,deleted
FROM user_dictionary_changes LEFT JOIN latest_resets USING(user_id),
LATERAL (SELECT change->'previous' AS item,true AS deleted,0 AS priority UNION ALL SELECT change->'replacement',false,1 UNION ALL SELECT value,false,2 FROM jsonb_array_elements(COALESCE(change->'ranking','[]'::jsonb))) AS c
WHERE item IS NOT NULL AND item<>'null'::jsonb
AND revision > COALESCE(reset_revision,0)
ORDER BY user_id,item->>'kind',item->>'code',item->>'word',revision DESC,priority DESC
ON CONFLICT DO NOTHING;
CREATE TABLE IF NOT EXISTS user_candidate_positions (
 user_id text NOT NULL REFERENCES auth_users(id) ON DELETE CASCADE,
 context text NOT NULL,
 code text NOT NULL,
 word text NOT NULL,
 position integer NOT NULL CHECK(position BETWEEN 1 AND 5),
 PRIMARY KEY(user_id,context,code,word),
 UNIQUE(user_id,context,position)
);

CREATE TABLE IF NOT EXISTS user_candidate_selections (
 user_id text NOT NULL REFERENCES auth_users(id) ON DELETE CASCADE,
 context text NOT NULL, code text NOT NULL, word text NOT NULL,
 count integer NOT NULL CHECK(count BETWEEN 0 AND 10),
 PRIMARY KEY(user_id,context,code,word)
);

-- 无编码常用语与偏好同一形状：整份列表一个 revision，PUT 时 CAS。
CREATE TABLE IF NOT EXISTS user_phrases (
 user_id text PRIMARY KEY REFERENCES auth_users(id) ON DELETE CASCADE,
 revision bigint NOT NULL DEFAULT 0,
 phrases jsonb NOT NULL DEFAULT '[]'
);

-- 云剪贴板的保留天数（0 表示一直保留，否则 1、7、30）、置顶和写入设备名。纯增量、带默认值：旧版本副本插入时不写这些列。
ALTER TABLE user_clipboard_settings ADD COLUMN IF NOT EXISTS retention_days integer NOT NULL DEFAULT 0;
ALTER TABLE user_clipboard ADD COLUMN IF NOT EXISTS pinned boolean NOT NULL DEFAULT false;
ALTER TABLE user_clipboard ADD COLUMN IF NOT EXISTS device text NOT NULL DEFAULT '';

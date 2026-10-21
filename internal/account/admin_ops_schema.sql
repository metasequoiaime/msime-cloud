-- 管理后台对其他业务表的迁移；依赖前面创建的社区和词库表，所有步骤均可重复执行。

-- Community moderation is post-moderation: new uploads are public at once with moderation='pending', and only 'removed' rows are hidden from the public endpoints. Rows that existed before moderation are approved. previous_moderation keeps the state a removal replaced, so restoring (or unbanning the owner) can put it back.
ALTER TABLE community_skins ADD COLUMN IF NOT EXISTS moderation text NOT NULL DEFAULT 'approved';
ALTER TABLE community_skins ADD COLUMN IF NOT EXISTS previous_moderation text;
ALTER TABLE community_skins ADD COLUMN IF NOT EXISTS moderation_reason text;
ALTER TABLE community_skins ADD COLUMN IF NOT EXISTS moderated_by text;
ALTER TABLE community_skins ADD COLUMN IF NOT EXISTS moderated_at timestamptz;
CREATE INDEX IF NOT EXISTS community_skins_pending ON community_skins(created_at) WHERE moderation='pending';

ALTER TABLE community_resources ADD COLUMN IF NOT EXISTS moderation text NOT NULL DEFAULT 'approved';
ALTER TABLE community_resources ADD COLUMN IF NOT EXISTS previous_moderation text;
ALTER TABLE community_resources ADD COLUMN IF NOT EXISTS moderation_reason text;
ALTER TABLE community_resources ADD COLUMN IF NOT EXISTS moderated_by text;
ALTER TABLE community_resources ADD COLUMN IF NOT EXISTS moderated_at timestamptz;
CREATE INDEX IF NOT EXISTS community_resources_pending ON community_resources(kind,created_at) WHERE moderation='pending';

ALTER TABLE community_candidate_skins ADD COLUMN IF NOT EXISTS moderation text NOT NULL DEFAULT 'approved';
ALTER TABLE community_candidate_skins ADD COLUMN IF NOT EXISTS previous_moderation text;
ALTER TABLE community_candidate_skins ADD COLUMN IF NOT EXISTS moderation_reason text;
ALTER TABLE community_candidate_skins ADD COLUMN IF NOT EXISTS moderated_by text;
ALTER TABLE community_candidate_skins ADD COLUMN IF NOT EXISTS moderated_at timestamptz;
CREATE INDEX IF NOT EXISTS community_candidate_skins_pending ON community_candidate_skins(created_at) WHERE moderation='pending';

ALTER TABLE community_plugins ADD COLUMN IF NOT EXISTS moderation text NOT NULL DEFAULT 'approved';
ALTER TABLE community_plugins ADD COLUMN IF NOT EXISTS previous_moderation text;
ALTER TABLE community_plugins ADD COLUMN IF NOT EXISTS moderation_reason text;
ALTER TABLE community_plugins ADD COLUMN IF NOT EXISTS moderated_by text;
ALTER TABLE community_plugins ADD COLUMN IF NOT EXISTS moderated_at timestamptz;
CREATE INDEX IF NOT EXISTS community_plugins_pending ON community_plugins(created_at) WHERE moderation='pending';

-- 社区举报的 kind 使用后台分区名；item_id 指向多张作品表，由删除触发器维持生命周期。
CREATE TABLE IF NOT EXISTS community_reports (
 id bigserial PRIMARY KEY,
 kind text NOT NULL CONSTRAINT community_reports_kind_known CHECK(kind IN ('skins','candidate-skins','plugins','dictionaries','replies','phrases')),
 item_id text NOT NULL CHECK(length(item_id) BETWEEN 1 AND 128),
 reporter_id text NOT NULL REFERENCES auth_users(id) ON DELETE CASCADE,
 reason text NOT NULL CHECK(length(reason) BETWEEN 1 AND 64),
 detail text NOT NULL DEFAULT '' CHECK(length(detail)<=1000),
 created_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(kind,item_id,reporter_id)
);
CREATE INDEX IF NOT EXISTS community_reports_item ON community_reports(kind,item_id);
-- 管理后台详情按内容查看最新举报；匹配条件和排序共用这个索引，避免每次只取 100 条时排序全部举报。
CREATE INDEX IF NOT EXISTS community_reports_item_newest ON community_reports(kind,item_id,created_at DESC,id DESC);
-- 账号注销按 reporter_id 触发外键级联；查询举报内容的索引以 kind,item_id 开头。
CREATE INDEX IF NOT EXISTS community_reports_reporter ON community_reports(reporter_id);
-- 举报的 kind 加入短语包 phrases：命名约束还不含它时，删掉列着 kind 取值的旧约束（早期是自动命名的）再加命名约束，可以重复执行。
DO $$
DECLARE c record;
BEGIN
 IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid='community_reports'::regclass AND conname='community_reports_kind_known' AND pg_get_constraintdef(oid) LIKE '%phrases%') THEN
  FOR c IN SELECT conname FROM pg_constraint WHERE conrelid='community_reports'::regclass AND contype='c' AND pg_get_constraintdef(oid) LIKE '%replies%' LOOP
   EXECUTE format('ALTER TABLE community_reports DROP CONSTRAINT %I',c.conname);
  END LOOP;
  ALTER TABLE community_reports ADD CONSTRAINT community_reports_kind_known CHECK(kind IN ('skins','candidate-skins','plugins','dictionaries','replies','phrases'));
 END IF;
END $$;

-- 举报跟随作品的生命周期；删除作者的级联操作和旧版本副本也必须同步清理。
CREATE OR REPLACE FUNCTION community_reports_cleanup() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE report_kind text;
BEGIN
 IF TG_TABLE_NAME='community_resources' THEN
  report_kind := CASE OLD.kind WHEN 'dictionary' THEN 'dictionaries' WHEN 'phrase' THEN 'phrases' ELSE 'replies' END;
 ELSE
  report_kind := TG_ARGV[0];
 END IF;
 -- 使用触发表所在的 schema，避免调用者的 search_path 把清理导向同名表。
 EXECUTE format('DELETE FROM %I.community_reports WHERE kind=$1 AND item_id=$2',TG_TABLE_SCHEMA) USING report_kind,OLD.id;
 RETURN OLD;
END $$;
DO $$
DECLARE item record;
BEGIN
 FOR item IN SELECT * FROM (VALUES
  ('community_skins','skins'),
  ('community_candidate_skins','candidate-skins'),
  ('community_plugins','plugins'),
  ('community_resources','resources')
 ) v(tbl,kind) LOOP
  IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgrelid=item.tbl::regclass AND tgname='community_reports_cleanup') THEN
   EXECUTE format('CREATE TRIGGER community_reports_cleanup AFTER DELETE ON %I FOR EACH ROW EXECUTE FUNCTION community_reports_cleanup(%L)',item.tbl,item.kind);
  END IF;
  EXECUTE format('ALTER TABLE %I ENABLE TRIGGER community_reports_cleanup',item.tbl);
 END LOOP;
END $$;
-- 旧版本留下的孤立举报不能被以后重用相同 ID 的新作品继承。
DELETE FROM community_reports r WHERE
 (kind='skins' AND NOT EXISTS(SELECT 1 FROM community_skins s WHERE s.id=r.item_id)) OR
 (kind='candidate-skins' AND NOT EXISTS(SELECT 1 FROM community_candidate_skins s WHERE s.id=r.item_id)) OR
 (kind='plugins' AND NOT EXISTS(SELECT 1 FROM community_plugins s WHERE s.id=r.item_id)) OR
 (kind IN ('dictionaries','replies','phrases') AND NOT EXISTS(SELECT 1 FROM community_resources s WHERE s.id=r.item_id AND s.kind=CASE r.kind WHEN 'dictionaries' THEN 'dictionary' WHEN 'phrases' THEN 'phrase' ELSE 'reply' END));

-- Account bans. A banned account cannot log in, refresh or use a session.
ALTER TABLE auth_users ADD COLUMN IF NOT EXISTS banned_at timestamptz;
ALTER TABLE auth_users ADD COLUMN IF NOT EXISTS ban_reason text;
ALTER TABLE auth_users ADD COLUMN IF NOT EXISTS banned_by text;
-- 独立计数只随封禁字段写入递增；常量默认值避免为新增列重写整张用户表。
ALTER TABLE auth_users ADD COLUMN IF NOT EXISTS ban_revision bigint NOT NULL DEFAULT 0;
CREATE OR REPLACE FUNCTION auth_user_ban_revision() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 NEW.ban_revision := OLD.ban_revision+1;
 RETURN NEW;
END $$;
DO $$ BEGIN
 IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgrelid='auth_users'::regclass AND tgname='auth_user_ban_revision') THEN
  CREATE TRIGGER auth_user_ban_revision BEFORE UPDATE OF banned_at,ban_reason,banned_by ON auth_users FOR EACH ROW EXECUTE FUNCTION auth_user_ban_revision();
 END IF;
END $$;
ALTER TABLE auth_users ENABLE TRIGGER auth_user_ban_revision;
CREATE INDEX IF NOT EXISTS auth_users_banned ON auth_users(banned_at) WHERE banned_at IS NOT NULL;
-- The User-Agent a session was created with, truncated, so the admin can tell a user's devices apart.
ALTER TABLE auth_sessions ADD COLUMN IF NOT EXISTS user_agent text NOT NULL DEFAULT '';

-- Website dictionary submissions, recorded after the GitHub write succeeded, so the review page can show each submission's note and time next to the rolling pull request.
CREATE TABLE IF NOT EXISTS word_submissions (
 id bigserial PRIMARY KEY,
 pr_number integer NOT NULL CHECK(pr_number>0),
 kind text NOT NULL CHECK(kind IN ('words','english','translations')),
 entries jsonb NOT NULL,
 note text NOT NULL DEFAULT '' CHECK(length(note)<=1000),
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS word_submissions_pr ON word_submissions(pr_number,created_at);

-- The CHECK constraints of the columns added above, by the names PostgreSQL gives a column constraint. They are added separately because PostgreSQL 12 adds an inline CHECK again on every rerun of ADD COLUMN IF NOT EXISTS even when the column exists; copies an earlier rerun left (name1, name2, ...) are dropped.
DO $$
DECLARE c record; d record;
BEGIN
 FOR c IN SELECT * FROM (VALUES
  ('community_skins','community_skins_moderation_check','moderation IN (''pending'',''approved'',''removed'')'),
  ('community_skins','community_skins_previous_moderation_check','previous_moderation IN (''pending'',''approved'')'),
  ('community_skins','community_skins_moderation_reason_check','length(moderation_reason)<=500'),
  ('community_resources','community_resources_moderation_check','moderation IN (''pending'',''approved'',''removed'')'),
  ('community_resources','community_resources_previous_moderation_check','previous_moderation IN (''pending'',''approved'')'),
  ('community_resources','community_resources_moderation_reason_check','length(moderation_reason)<=500'),
  ('community_candidate_skins','community_candidate_skins_moderation_check','moderation IN (''pending'',''approved'',''removed'')'),
  ('community_candidate_skins','community_candidate_skins_previous_moderation_check','previous_moderation IN (''pending'',''approved'')'),
  ('community_candidate_skins','community_candidate_skins_moderation_reason_check','length(moderation_reason)<=500'),
  ('community_plugins','community_plugins_moderation_check','moderation IN (''pending'',''approved'',''removed'')'),
  ('community_plugins','community_plugins_previous_moderation_check','previous_moderation IN (''pending'',''approved'')'),
  ('community_plugins','community_plugins_moderation_reason_check','length(moderation_reason)<=500'),
  ('auth_users','auth_users_ban_reason_check','length(ban_reason)<=500'),
  ('auth_sessions','auth_sessions_user_agent_check','length(user_agent)<=256')
 ) v(tbl,name,expr) LOOP
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid=c.tbl::regclass AND conname=c.name) THEN
   EXECUTE format('ALTER TABLE %I ADD CONSTRAINT %I CHECK(%s)',c.tbl,c.name,c.expr);
  END IF;
  FOR d IN SELECT conname FROM pg_constraint WHERE conrelid=c.tbl::regclass AND contype='c' AND conname ~ ('^'||c.name||'[0-9]+$') LOOP
   EXECUTE format('ALTER TABLE %I DROP CONSTRAINT %I',c.tbl,d.conname);
  END LOOP;
 END LOOP;
END $$;

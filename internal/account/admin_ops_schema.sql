-- Admin console changes to tables outside the admin_* family. This file runs last in the migration because it alters the community and dictionary tables created by the files before it. Every statement is additive and idempotent.

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

-- User reports on community content. kind uses the admin section names; item_id has no foreign key because it points into one of five tables.
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

-- Account bans. A banned account cannot log in, refresh or use a session.
ALTER TABLE auth_users ADD COLUMN IF NOT EXISTS banned_at timestamptz;
ALTER TABLE auth_users ADD COLUMN IF NOT EXISTS ban_reason text;
ALTER TABLE auth_users ADD COLUMN IF NOT EXISTS banned_by text;
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

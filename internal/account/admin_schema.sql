CREATE TABLE IF NOT EXISTS admin_events (
 id text PRIMARY KEY,
 kind text NOT NULL CONSTRAINT admin_events_kind_check CHECK(kind IN ('download','crash','active','session','session_crash')),
 platform text NOT NULL,
 version text NOT NULL,
 message text NOT NULL DEFAULT '',
 stack text NOT NULL DEFAULT '',
 resolved boolean NOT NULL DEFAULT false,
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS admin_events_kind_time ON admin_events(kind,created_at DESC,id);
CREATE TABLE IF NOT EXISTS admin_audit (
 id bigserial PRIMARY KEY,
 action text NOT NULL,
 target text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now()
);
-- 审计列表按时间和 ID 倒序分页，避免每次选页都扫描并排序全部历史。
CREATE INDEX IF NOT EXISTS admin_audit_time ON admin_audit(created_at DESC,id DESC);
-- 反馈撤销按目标查最近一次操作，同值设置也通过审计更新版本。
CREATE INDEX IF NOT EXISTS admin_audit_feedback_version ON admin_audit(target,id DESC) WHERE action IN ('resolve_feedback','reopen_feedback','undo_feedback_status');

CREATE INDEX IF NOT EXISTS auth_users_created ON auth_users(created_at);

CREATE TABLE IF NOT EXISTS admin_login_flows (
 state_hash text PRIMARY KEY, nonce text NOT NULL, verifier text NOT NULL,
 expires_at timestamptz NOT NULL DEFAULT now()+interval '10 minutes'
);
CREATE INDEX IF NOT EXISTS admin_login_flows_expiry ON admin_login_flows(expires_at);
CREATE TABLE IF NOT EXISTS admin_sessions (
 token_hash text PRIMARY KEY, subject text NOT NULL, email text NOT NULL,
 expires_at timestamptz NOT NULL DEFAULT now()+interval '8 hours'
);
CREATE INDEX IF NOT EXISTS admin_sessions_expiry ON admin_sessions(expires_at);
ALTER TABLE admin_audit ADD COLUMN IF NOT EXISTS actor text NOT NULL DEFAULT 'legacy-token';

CREATE TABLE IF NOT EXISTS admin_members (
 email text PRIMARY KEY CHECK(email=lower(email)),
 enabled boolean NOT NULL DEFAULT true,
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now()
);

-- Admin console. Every statement below is additive and idempotent: the startup readiness probes re-run this file whenever one of these objects is missing.
ALTER TABLE admin_audit ADD COLUMN IF NOT EXISTS detail jsonb NOT NULL DEFAULT '{}';

-- Role-based access. Owners from the deployment configuration and the legacy token never need a row here; members get their permissions through admin_members.role.
CREATE TABLE IF NOT EXISTS admin_roles (
 key text PRIMARY KEY CHECK(key ~ '^[a-z][a-z0-9_]{0,31}$'),
 name text NOT NULL CHECK(length(name) BETWEEN 1 AND 32),
 builtin boolean NOT NULL DEFAULT false
);
CREATE TABLE IF NOT EXISTS admin_role_permissions (
 role text NOT NULL REFERENCES admin_roles(key) ON DELETE CASCADE,
 permission text NOT NULL CONSTRAINT admin_role_permissions_permission_check CHECK(permission IN ('review_dict_pr','review_community','triage_issues','ban_users','publish_notices','trigger_release','view_cloud_usage','view_logs','manage_permissions')),
 PRIMARY KEY(role,permission)
);
-- 服务日志权限 view_logs 是后加的：旧库的 CHECK 约束不认识它。只在约束还是旧版时替换约束并给维护者授予一次，之后在后台收回也不会被重跑的迁移补回。
DO $$ BEGIN
 IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid='admin_role_permissions'::regclass AND conname='admin_role_permissions_permission_check' AND pg_get_constraintdef(oid) LIKE '%view_logs%') THEN
  ALTER TABLE admin_role_permissions DROP CONSTRAINT IF EXISTS admin_role_permissions_permission_check;
  ALTER TABLE admin_role_permissions ADD CONSTRAINT admin_role_permissions_permission_check CHECK(permission IN ('review_dict_pr','review_community','triage_issues','ban_users','publish_notices','trigger_release','view_cloud_usage','view_logs','manage_permissions'));
  INSERT INTO admin_role_permissions(role,permission) SELECT 'maintainer','view_logs' WHERE EXISTS (SELECT 1 FROM admin_roles WHERE key='maintainer') ON CONFLICT DO NOTHING;
 END IF;
END $$;
-- A built-in role gets its default permissions only in the statement that creates the role row, so a matrix edited later through the console is never reset by a re-run migration.
WITH created AS (
 INSERT INTO admin_roles(key,name,builtin) VALUES ('maintainer','维护者',true),('reviewer','审核志愿者',true),('operator','运营/客服',true),('readonly','只读',true)
 ON CONFLICT(key) DO NOTHING RETURNING key
)
INSERT INTO admin_role_permissions(role,permission)
SELECT d.role,d.permission FROM created c JOIN (VALUES
 ('maintainer','review_dict_pr'),('maintainer','review_community'),('maintainer','triage_issues'),('maintainer','ban_users'),('maintainer','publish_notices'),('maintainer','trigger_release'),('maintainer','view_cloud_usage'),('maintainer','view_logs'),('maintainer','manage_permissions'),
 ('reviewer','review_dict_pr'),('reviewer','review_community'),('reviewer','triage_issues'),
 ('operator','triage_issues'),('operator','ban_users'),('operator','publish_notices'),('operator','view_cloud_usage'),
 ('readonly','view_cloud_usage')
) d(role,permission) ON d.role=c.key
ON CONFLICT DO NOTHING;
-- Maintainers can never lose permission management; the console refuses that revocation, and this restores it should the row be removed by hand.
INSERT INTO admin_role_permissions(role,permission) VALUES ('maintainer','manage_permissions') ON CONFLICT DO NOTHING;
-- Members added before roles existed could do everything but manage admins, so they become maintainers.
ALTER TABLE admin_members ADD COLUMN IF NOT EXISTS role text NOT NULL DEFAULT 'maintainer';
-- Added separately: a REFERENCES clause inside ADD COLUMN IF NOT EXISTS is not guarded by IF NOT EXISTS on every server version.
DO $$ BEGIN
 IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid='admin_members'::regclass AND conname='admin_members_role_fkey') THEN
  ALTER TABLE admin_members ADD CONSTRAINT admin_members_role_fkey FOREIGN KEY(role) REFERENCES admin_roles(key);
 END IF;
END $$;

-- Session metadata for the personal page. id is a short random handle the console can show and revoke by, because the token hash must never leave the server. It is built from md5 rather than gen_random_uuid(), which is core only from PostgreSQL 13 while the documented minimum is 12.
ALTER TABLE admin_sessions ADD COLUMN IF NOT EXISTS id text NOT NULL DEFAULT left(md5(random()::text||clock_timestamp()::text),16);
ALTER TABLE admin_sessions ADD COLUMN IF NOT EXISTS name text NOT NULL DEFAULT '';
ALTER TABLE admin_sessions ADD COLUMN IF NOT EXISTS created_at timestamptz NOT NULL DEFAULT now();
ALTER TABLE admin_sessions ADD COLUMN IF NOT EXISTS last_seen_at timestamptz NOT NULL DEFAULT now();
ALTER TABLE admin_sessions ADD COLUMN IF NOT EXISTS user_agent text NOT NULL DEFAULT '';
CREATE UNIQUE INDEX IF NOT EXISTS admin_sessions_id ON admin_sessions(id);
CREATE INDEX IF NOT EXISTS admin_sessions_email ON admin_sessions(email);

-- Personal access tokens (Authorization: Bearer msime_pat_...). Only the SHA-256 of the token is stored; the holder's admin membership is re-checked on every request.
CREATE TABLE IF NOT EXISTS admin_tokens (
 hash text PRIMARY KEY,
 email text NOT NULL CHECK(email=lower(email)),
 last4 text NOT NULL CHECK(length(last4)=4),
 created_at timestamptz NOT NULL DEFAULT now(),
 expires_at timestamptz NOT NULL DEFAULT now()+interval '30 days'
);
CREATE INDEX IF NOT EXISTS admin_tokens_email ON admin_tokens(email);
CREATE INDEX IF NOT EXISTS admin_tokens_expiry ON admin_tokens(expires_at);

-- Telemetry extensions. All optional, so older clients keep working: artifact and channel describe a download, install_id is an anonymous per-install identifier, signature groups crashes.
ALTER TABLE admin_events ADD COLUMN IF NOT EXISTS artifact text;
ALTER TABLE admin_events ADD COLUMN IF NOT EXISTS channel text;
ALTER TABLE admin_events ADD COLUMN IF NOT EXISTS install_id text;
ALTER TABLE admin_events ADD COLUMN IF NOT EXISTS signature text;
-- The CHECK constraints of the columns added above, by the names PostgreSQL gives a column constraint. They are added separately because PostgreSQL 12 adds an inline CHECK again on every rerun of ADD COLUMN IF NOT EXISTS even when the column exists; copies an earlier rerun left (name1, name2, ...) are dropped.
DO $$
DECLARE c record; d record;
BEGIN
 FOR c IN SELECT * FROM (VALUES
  ('admin_sessions','admin_sessions_name_check','length(name)<=200'),
  ('admin_sessions','admin_sessions_user_agent_check','length(user_agent)<=256'),
  ('admin_events','admin_events_artifact_check','length(artifact) BETWEEN 1 AND 64'),
  ('admin_events','admin_events_channel_check','length(channel) BETWEEN 1 AND 32'),
  ('admin_events','admin_events_install_id_check','length(install_id) BETWEEN 16 AND 64'),
  ('admin_events','admin_events_signature_check','signature ~ ''^[0-9a-f]{16}$''')
 ) v(tbl,name,expr) LOOP
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid=c.tbl::regclass AND conname=c.name) THEN
   EXECUTE format('ALTER TABLE %I ADD CONSTRAINT %I CHECK(%s)',c.tbl,c.name,c.expr);
  END IF;
  FOR d IN SELECT conname FROM pg_constraint WHERE conrelid=c.tbl::regclass AND contype='c' AND conname ~ ('^'||c.name||'[0-9]+$') LOOP
   EXECUTE format('ALTER TABLE %I DROP CONSTRAINT %I',c.tbl,d.conname);
  END LOOP;
 END LOOP;
END $$;
DO $$ BEGIN
 IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid='admin_events'::regclass AND conname='admin_events_kind_check' AND pg_get_constraintdef(oid) LIKE '%session_crash%') THEN
  ALTER TABLE admin_events DROP CONSTRAINT IF EXISTS admin_events_kind_check;
  ALTER TABLE admin_events ADD CONSTRAINT admin_events_kind_check CHECK(kind IN ('download','crash','active','session','session_crash'));
 END IF;
END $$;
CREATE INDEX IF NOT EXISTS admin_events_signature ON admin_events(signature,created_at DESC) WHERE signature IS NOT NULL;
-- The startup backfill walks unsigned crashes by (created_at,id); this keeps it an index scan of the few rows left instead of a scan of every crash.
CREATE INDEX IF NOT EXISTS admin_events_unsigned_crash ON admin_events(created_at,id) WHERE kind='crash' AND signature IS NULL;

CREATE TABLE IF NOT EXISTS admin_crash_groups (
 signature text PRIMARY KEY CHECK(signature ~ '^[0-9a-f]{16}$'),
 platform text NOT NULL,
 version text NOT NULL,
 title text NOT NULL,
 status text NOT NULL DEFAULT 'open' CHECK(status IN ('open','known','fixed')),
 issue_url text,
 first_seen timestamptz NOT NULL DEFAULT now(),
 last_seen timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS admin_crash_groups_seen ON admin_crash_groups(last_seen DESC);

-- 状态版本独立于上报统计；默认值区分删除重建，触发器也覆盖滚动升级期间旧副本的写入。
ALTER TABLE admin_crash_groups ADD COLUMN IF NOT EXISTS status_token text NOT NULL DEFAULT md5(random()::text||clock_timestamp()::text);
CREATE OR REPLACE FUNCTION admin_crash_status_token() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 NEW.status_token := md5(random()::text||clock_timestamp()::text);
 RETURN NEW;
END $$;
DO $$ BEGIN
 IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgrelid='admin_crash_groups'::regclass AND tgname='admin_crash_status_token') THEN
  CREATE TRIGGER admin_crash_status_token BEFORE UPDATE OF status,issue_url ON admin_crash_groups FOR EACH ROW EXECUTE FUNCTION admin_crash_status_token();
 END IF;
END $$;
ALTER TABLE admin_crash_groups ENABLE TRIGGER admin_crash_status_token;

CREATE TABLE IF NOT EXISTS admin_notices (
 id bigserial PRIMARY KEY,
 title text NOT NULL DEFAULT '' CHECK(length(title)<=200),
 body text NOT NULL DEFAULT '' CHECK(length(body)<=20000),
 targets text[] NOT NULL DEFAULT '{}',
 channels text[] NOT NULL DEFAULT '{}',
 status text NOT NULL DEFAULT 'draft' CHECK(status IN ('draft','live','archived')),
 created_by text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 published_at timestamptz,
 updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS admin_notices_live ON admin_notices(published_at DESC) WHERE status='live';

CREATE TABLE IF NOT EXISTS admin_notifications (
 id bigserial PRIMARY KEY,
 kind text NOT NULL CHECK(length(kind) BETWEEN 1 AND 32),
 title text NOT NULL CHECK(length(title) BETWEEN 1 AND 300),
 target_page text NOT NULL DEFAULT '' CHECK(length(target_page)<=32),
 target_id text NOT NULL DEFAULT '' CHECK(length(target_id)<=200),
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS admin_notifications_time ON admin_notifications(created_at DESC,id DESC);
-- 按类型和目标查重，并在同一目标内按时间检查崩溃通知的静默期；允许同一目标有多条通知。
CREATE INDEX IF NOT EXISTS admin_notifications_target ON admin_notifications(kind,target_id,created_at DESC);
CREATE TABLE IF NOT EXISTS admin_notification_reads (
 email text NOT NULL CHECK(email=lower(email)),
 notification_id bigint NOT NULL REFERENCES admin_notifications(id) ON DELETE CASCADE,
 PRIMARY KEY(email,notification_id)
);
CREATE TABLE IF NOT EXISTS admin_preferences (
 email text PRIMARY KEY CHECK(email=lower(email)),
 prefs jsonb NOT NULL DEFAULT '{}',
 read_all_before timestamptz
);

-- Upstream service monitoring: hourly call buckets, a daily availability rollup kept for 60 days, and incidents.
CREATE TABLE IF NOT EXISTS admin_service_metrics (
 service text NOT NULL,
 hour timestamptz NOT NULL,
 calls bigint NOT NULL DEFAULT 0,
 errors bigint NOT NULL DEFAULT 0,
 latency_buckets jsonb NOT NULL DEFAULT '{}',
 usage numeric NOT NULL DEFAULT 0,
 PRIMARY KEY(service,hour)
);
CREATE TABLE IF NOT EXISTS admin_service_daily (
 service text NOT NULL,
 day date NOT NULL,
 ok_minutes integer NOT NULL DEFAULT 0,
 total_minutes integer NOT NULL DEFAULT 0,
 degraded boolean NOT NULL DEFAULT false,
 p95_ms integer,
 PRIMARY KEY(service,day)
);

-- 邮件投递按 UTC 日期汇总，保留供应商实际调用和结果，不把额度预留当作发信成功。
CREATE TABLE IF NOT EXISTS admin_email_delivery_daily (
 day date PRIMARY KEY,
 attempted bigint NOT NULL DEFAULT 0,
 accepted bigint NOT NULL DEFAULT 0,
 rejected bigint NOT NULL DEFAULT 0,
 failed bigint NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS admin_incidents (
 id bigserial PRIMARY KEY,
 service text NOT NULL,
 title text NOT NULL CHECK(length(title) BETWEEN 1 AND 200),
 description text NOT NULL DEFAULT '' CHECK(length(description)<=5000),
 state text NOT NULL DEFAULT 'open' CHECK(state IN ('open','resolved')),
 started_at timestamptz NOT NULL DEFAULT now(),
 resolved_at timestamptz,
 auto boolean NOT NULL DEFAULT false,
 CHECK((state='resolved')=(resolved_at IS NOT NULL))
);
CREATE INDEX IF NOT EXISTS admin_incidents_time ON admin_incidents(started_at DESC);
-- At most one automatically opened incident per service is open at a time.
CREATE UNIQUE INDEX IF NOT EXISTS admin_incidents_auto_open ON admin_incidents(service) WHERE state='open' AND auto;
-- Per-minute call counts and latency histograms that every replica adds its own calls to, so the status probe judges a service by the traffic of all replicas. Kept for a few hours.
CREATE TABLE IF NOT EXISTS admin_service_minutes (
 minute timestamptz NOT NULL,
 service text NOT NULL,
 calls bigint NOT NULL DEFAULT 0,
 errors bigint NOT NULL DEFAULT 0,
 latency_buckets jsonb NOT NULL DEFAULT '{}',
 PRIMARY KEY(minute,service)
);
-- One status verdict per service and minute, written only by the replica that leads the status probe. The primary key makes each minute count once in admin_service_daily; the newest minute is the status every replica reports; bad_runs, good_runs and incident carry the incident streaks across a change of leader. Kept for a few hours.
CREATE TABLE IF NOT EXISTS admin_service_verdicts (
 minute timestamptz NOT NULL,
 service text NOT NULL,
 state text NOT NULL CHECK(state IN ('ok','idle','degraded','down')),
 calls bigint NOT NULL DEFAULT 0,
 errors bigint NOT NULL DEFAULT 0,
 p95_ms integer,
 bad_runs integer NOT NULL DEFAULT 0,
 good_runs integer NOT NULL DEFAULT 0,
 incident boolean NOT NULL DEFAULT false,
 checked_at timestamptz NOT NULL,
 PRIMARY KEY(minute,service)
);

-- Daily download_count snapshots of GitHub Release assets; a day's downloads are the difference between adjacent snapshots.
CREATE TABLE IF NOT EXISTS release_asset_snapshots (
 repo text NOT NULL,
 tag text NOT NULL,
 asset text NOT NULL,
 day date NOT NULL,
 download_count bigint NOT NULL CHECK(download_count>=0),
 PRIMARY KEY(repo,tag,asset,day)
);
CREATE INDEX IF NOT EXISTS release_asset_snapshots_day ON release_asset_snapshots(day);

-- Sensitive words screen community uploads and dictionary submissions. Hits are counted per word and day.
CREATE TABLE IF NOT EXISTS admin_sensitive_words (
 id bigserial PRIMARY KEY,
 pattern text NOT NULL UNIQUE CHECK(length(pattern) BETWEEN 1 AND 200),
 is_regex boolean NOT NULL DEFAULT false,
 category text NOT NULL CHECK(category IN ('ad','vulgar','abuse','illegal','custom')),
 level text NOT NULL CHECK(level IN ('block','review')),
 created_by text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS admin_sensitive_hits (
 word_id bigint NOT NULL REFERENCES admin_sensitive_words(id) ON DELETE CASCADE,
 day date NOT NULL,
 count bigint NOT NULL DEFAULT 0 CHECK(count>=0),
 PRIMARY KEY(word_id,day)
);

-- Website settings edited in the admin console and read publicly by the official site (for example the Lanzou mirror link on the download page). Clearing a setting keeps the row with an empty value so the last editor stays visible.
CREATE TABLE IF NOT EXISTS site_settings (
 key text PRIMARY KEY,
 value text NOT NULL,
 updated_at timestamptz NOT NULL DEFAULT now(),
 updated_by text NOT NULL
);

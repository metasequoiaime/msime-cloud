-- User-published candidate-window skin packages (skin.toml plus re-encoded png/jpeg). They are separate from the curated candidate_skins tables, so uploads never reach /v1/skins, whose catalog fails whole above 256 rows. id is the client's publication UUID. package_id is the manifest id and the install folder on clients. A private row is the author's own synced copy: only its owner sees it, and it may omit the asset license a public row must declare.
CREATE TABLE IF NOT EXISTS community_candidate_skins (
 id text PRIMARY KEY CHECK(id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
 owner_id text NOT NULL REFERENCES auth_users(id) ON DELETE CASCADE,
 package_id text NOT NULL CHECK(package_id ~ '^[a-z0-9][a-z0-9._-]{0,63}$' AND package_id NOT IN ('system','shuishan','light','paper','night','ink','custom','fluent','wechat','graphite','willow_green','autumn_osmanthus','microsoft','default')),
 name text NOT NULL,
 description text NOT NULL DEFAULT '',
 version text NOT NULL,
 license_code text NOT NULL DEFAULT '',
 license_assets text NOT NULL,
 license_source text NOT NULL DEFAULT '',
 manifest bytea NOT NULL CHECK(octet_length(manifest) BETWEEN 1 AND 65536),
 preview_path text NOT NULL,
 -- sha256 over the canonical upload (name, description, manifest, then each path with the sha256 of its original bytes). Retries compare this rather than stored bytes, because stored images are re-encoded.
 request_sha256 text NOT NULL CHECK(request_sha256 ~ '^[0-9a-f]{64}$'),
 visibility text NOT NULL DEFAULT 'public' CHECK(visibility IN ('private','public')),
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(owner_id,id)
);
-- Databases created before private rows existed: every existing row stays public, and the column-level license check that required assets on every row is replaced by the named one below.
ALTER TABLE community_candidate_skins ADD COLUMN IF NOT EXISTS visibility text NOT NULL DEFAULT 'public';
-- 社区图库的分类，只是发布元数据，不属于 skin.toml。已有行迁移后为 other。取值与 Go 中的 candidateSkinCategories 一致，约束在下面的循环里以命名约束单独添加。
ALTER TABLE community_candidate_skins ADD COLUMN IF NOT EXISTS category text NOT NULL DEFAULT 'other';
-- The CHECK constraints of the columns added above, by the names PostgreSQL gives a column constraint. They are added separately because PostgreSQL 12 adds an inline CHECK again on every rerun of ADD COLUMN IF NOT EXISTS even when the column exists; copies an earlier rerun left (name1, name2, ...) are dropped.
DO $$
DECLARE c record; d record;
BEGIN
 FOR c IN SELECT * FROM (VALUES
  ('community_candidate_skins','community_candidate_skins_visibility_check','visibility IN (''private'',''public'')'),
  ('community_candidate_skins','community_candidate_skins_category_check','category IN (''nature'',''guofeng'',''acg'',''cute'',''food'',''tech'',''minimal'',''other'')')
 ) v(tbl,name,expr) LOOP
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid=c.tbl::regclass AND conname=c.name) THEN
   EXECUTE format('ALTER TABLE %I ADD CONSTRAINT %I CHECK(%s)',c.tbl,c.name,c.expr);
  END IF;
  FOR d IN SELECT conname FROM pg_constraint WHERE conrelid=c.tbl::regclass AND contype='c' AND conname ~ ('^'||c.name||'[0-9]+$') LOOP
   EXECUTE format('ALTER TABLE %I DROP CONSTRAINT %I',c.tbl,d.conname);
  END LOOP;
 END LOOP;
END $$;
-- A released row was last changed when it was published, so updated_at starts from created_at rather than from the migration time.
DO $$
BEGIN
 IF NOT EXISTS (SELECT 1 FROM pg_attribute WHERE attrelid='community_candidate_skins'::regclass AND attname='updated_at' AND NOT attisdropped) THEN
  ALTER TABLE community_candidate_skins ADD COLUMN updated_at timestamptz NOT NULL DEFAULT now();
  UPDATE community_candidate_skins SET updated_at=created_at;
 END IF;
END $$;
ALTER TABLE community_candidate_skins DROP CONSTRAINT IF EXISTS community_candidate_skins_license_assets_check;
DO $$
BEGIN
 IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid='community_candidate_skins'::regclass AND conname='community_candidate_skins_license_check') THEN
  ALTER TABLE community_candidate_skins ADD CONSTRAINT community_candidate_skins_license_check CHECK(length(btrim(license_assets)) <= 120 AND (visibility='private' OR length(btrim(license_assets)) >= 1));
 END IF;
END $$;
-- 保留 ID 与客户端的 is_reserved 一致，做法同 candidate_skin_schema.sql：旧库的列约束不含 autumn_osmanthus、microsoft 与 default 时整体替换，以 NOT VALID 添加，已有行不回头校验，新发布的行照常受约束。
DO $$
BEGIN
 IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid='community_candidate_skins'::regclass AND conname='community_candidate_skins_package_id_check' AND pg_get_constraintdef(oid) LIKE '%microsoft%') THEN
  ALTER TABLE community_candidate_skins DROP CONSTRAINT IF EXISTS community_candidate_skins_package_id_check;
  ALTER TABLE community_candidate_skins ADD CONSTRAINT community_candidate_skins_package_id_check CHECK(package_id ~ '^[a-z0-9][a-z0-9._-]{0,63}$' AND package_id NOT IN ('system','shuishan','light','paper','night','ink','custom','fluent','wechat','graphite','willow_green','autumn_osmanthus','microsoft','default')) NOT VALID;
 END IF;
END $$;
CREATE INDEX IF NOT EXISTS community_candidate_skins_newest ON community_candidate_skins(created_at DESC,id);
CREATE INDEX IF NOT EXISTS community_candidate_skins_owner ON community_candidate_skins(owner_id,created_at DESC);
-- 按分类筛选的公开目录沿用 community_candidate_skins_newest 的排序（created_at DESC,id）。
CREATE INDEX IF NOT EXISTS community_candidate_skins_category_newest ON community_candidate_skins(category,created_at DESC,id);
-- One row per re-encoded image. size and sha256 are generated, so they cannot disagree with the bytes download serves.
CREATE TABLE IF NOT EXISTS community_candidate_skin_files (
 skin_id text NOT NULL REFERENCES community_candidate_skins(id) ON DELETE CASCADE,
 path text NOT NULL CHECK(length(path) <= 256 AND path ~ '^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*$' AND path !~ '(^|/)\.\.?(/|$)' AND lower(path) ~ '\.(png|jpe?g)$'),
 bytes bytea NOT NULL CHECK(octet_length(bytes) BETWEEN 1 AND 1048576),
 size integer GENERATED ALWAYS AS (octet_length(bytes)) STORED,
 sha256 text GENERATED ALWAYS AS (encode(sha256(bytes),'hex')) STORED,
 PRIMARY KEY(skin_id,path)
);
CREATE TABLE IF NOT EXISTS community_candidate_skin_downloads (
 skin_id text NOT NULL REFERENCES community_candidate_skins(id) ON DELETE CASCADE,
 user_id text NOT NULL REFERENCES auth_users(id) ON DELETE CASCADE,
 PRIMARY KEY(skin_id,user_id)
);
CREATE TABLE IF NOT EXISTS community_candidate_skin_ratings (
 skin_id text NOT NULL REFERENCES community_candidate_skins(id) ON DELETE CASCADE,
 user_id text NOT NULL REFERENCES auth_users(id) ON DELETE CASCADE,
 stars integer NOT NULL CHECK(stars BETWEEN 1 AND 5),
 PRIMARY KEY(skin_id,user_id)
);
-- 候选窗皮肤的收藏，与 community_skin_saves 相同：created_at 是收藏时间，`scope=saved` 按它倒序列出。
CREATE TABLE IF NOT EXISTS community_candidate_skin_saves (
 skin_id text NOT NULL REFERENCES community_candidate_skins(id) ON DELETE CASCADE,
 user_id text NOT NULL REFERENCES auth_users(id) ON DELETE CASCADE,
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(skin_id,user_id)
);
CREATE INDEX IF NOT EXISTS community_candidate_skin_saves_user ON community_candidate_skin_saves(user_id,created_at DESC);

-- Community designs belong to accounts; unique keys make counters safe across replicas.
CREATE TABLE IF NOT EXISTS community_skins (
 id text PRIMARY KEY,
 owner_id text NOT NULL REFERENCES auth_users(id) ON DELETE CASCADE,
 name text NOT NULL,
 description text NOT NULL DEFAULT '',
 design jsonb NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(owner_id,id)
);
CREATE INDEX IF NOT EXISTS community_skins_newest ON community_skins(created_at DESC,id);
-- 社区键盘皮肤的图库分类，只是发布元数据，不属于 design。已有行迁移后为 other。取值与 Go 中的 candidateSkinCategories 一致（与候选窗皮肤共用同一组分类）。约束在下面的循环里以命名约束单独添加，因为 PostgreSQL 12 每次重跑 ADD COLUMN IF NOT EXISTS 都会再加一份行内 CHECK；之前重跑留下的副本（name1、name2……）会被删掉。
ALTER TABLE community_skins ADD COLUMN IF NOT EXISTS category text NOT NULL DEFAULT 'other';
DO $$
DECLARE c record; d record;
BEGIN
 FOR c IN SELECT * FROM (VALUES
  ('community_skins','community_skins_category_check','category IN (''nature'',''guofeng'',''acg'',''cute'',''food'',''tech'',''minimal'',''other'')')
 ) v(tbl,name,expr) LOOP
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid=c.tbl::regclass AND conname=c.name) THEN
   EXECUTE format('ALTER TABLE %I ADD CONSTRAINT %I CHECK(%s)',c.tbl,c.name,c.expr);
  END IF;
  FOR d IN SELECT conname FROM pg_constraint WHERE conrelid=c.tbl::regclass AND contype='c' AND conname ~ ('^'||c.name||'[0-9]+$') LOOP
   EXECUTE format('ALTER TABLE %I DROP CONSTRAINT %I',c.tbl,d.conname);
  END LOOP;
 END LOOP;
END $$;
-- 按分类筛选的公开目录沿用 community_skins_newest 的排序（created_at DESC,id）。
CREATE INDEX IF NOT EXISTS community_skins_category_newest ON community_skins(category,created_at DESC,id);
CREATE TABLE IF NOT EXISTS community_skin_downloads (
 skin_id text NOT NULL REFERENCES community_skins(id) ON DELETE CASCADE,
 user_id text NOT NULL REFERENCES auth_users(id) ON DELETE CASCADE,
 PRIMARY KEY(skin_id,user_id)
);
CREATE TABLE IF NOT EXISTS community_skin_ratings (
 skin_id text NOT NULL REFERENCES community_skins(id) ON DELETE CASCADE,
 user_id text NOT NULL REFERENCES auth_users(id) ON DELETE CASCADE,
 stars integer NOT NULL CHECK(stars BETWEEN 1 AND 5),
 PRIMARY KEY(skin_id,user_id)
);
-- 键盘皮肤的收藏，每个账号每款最多一条。created_at 是收藏时间，`scope=saved` 按它倒序列出，所以按 (user_id,created_at) 建索引。
CREATE TABLE IF NOT EXISTS community_skin_saves (
 skin_id text NOT NULL REFERENCES community_skins(id) ON DELETE CASCADE,
 user_id text NOT NULL REFERENCES auth_users(id) ON DELETE CASCADE,
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(skin_id,user_id)
);
CREATE INDEX IF NOT EXISTS community_skin_saves_user ON community_skin_saves(user_id,created_at DESC);

-- 带版本的纯数据社区资源：词包、回复提示词和短语包。不暴露任何个人词库。
CREATE TABLE IF NOT EXISTS community_resources (
 id text PRIMARY KEY,
 owner_id text NOT NULL REFERENCES auth_users(id) ON DELETE CASCADE,
 kind text NOT NULL CONSTRAINT community_resources_kind_known CHECK(kind IN ('dictionary','reply','phrase')),
 name text NOT NULL,
 description text NOT NULL DEFAULT '',
 content jsonb NOT NULL,
 revision integer NOT NULL DEFAULT 1 CHECK(revision > 0),
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS community_resources_catalog ON community_resources(kind,created_at DESC,id);
-- 早期建的表用的是自动命名的 kind 约束（不含 phrase）。命名约束还不含 phrase 时，按 pg_constraint 查出列着 kind 取值的旧约束逐个删掉，再加上命名约束；整段在迁移的 advisory lock 下执行，可以重复执行。以后再加类型时把这里探测的类型名换成新加的那个，并同步 store.go 的 Ready。
DO $$
DECLARE c record;
BEGIN
 IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid='community_resources'::regclass AND conname='community_resources_kind_known' AND pg_get_constraintdef(oid) LIKE '%phrase%') THEN
  FOR c IN SELECT conname FROM pg_constraint WHERE conrelid='community_resources'::regclass AND contype='c' AND pg_get_constraintdef(oid) LIKE '%kind%' AND pg_get_constraintdef(oid) LIKE '%reply%' LOOP
   EXECUTE format('ALTER TABLE community_resources DROP CONSTRAINT %I',c.conname);
  END LOOP;
  ALTER TABLE community_resources ADD CONSTRAINT community_resources_kind_known CHECK(kind IN ('dictionary','reply','phrase'));
 END IF;
END $$;
CREATE TABLE IF NOT EXISTS community_resource_saves (
 resource_id text NOT NULL REFERENCES community_resources(id) ON DELETE CASCADE,
 user_id text NOT NULL REFERENCES auth_users(id) ON DELETE CASCADE,
 PRIMARY KEY(resource_id,user_id)
);
CREATE TABLE IF NOT EXISTS community_resource_ratings (
 resource_id text NOT NULL REFERENCES community_resources(id) ON DELETE CASCADE,
 user_id text NOT NULL REFERENCES auth_users(id) ON DELETE CASCADE,
 stars integer NOT NULL CHECK(stars BETWEEN 1 AND 5),
 PRIMARY KEY(resource_id,user_id)
);

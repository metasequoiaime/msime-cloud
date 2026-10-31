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
-- UNIQUE(owner_id,id) 已覆盖作者筛选；删除旧版本曾经加上的重复索引。
DO $$
DECLARE idx record;
BEGIN
 FOR idx IN SELECT n.nspname,c.relname FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid JOIN pg_namespace n ON n.oid=c.relnamespace
  WHERE i.indrelid='community_skins'::regclass AND c.relname='community_skins_owner' LOOP
  EXECUTE format('DROP INDEX %I.%I',idx.nspname,idx.relname);
 END LOOP;
END $$;
-- 社区键盘皮肤的图库分类，只是发布元数据，不属于 design。已有行迁移后为 other。取值与 Go 中的 candidateSkinCategories 一致（与候选窗皮肤共用同一组分类）。约束在下面的循环里以命名约束单独添加，因为 PostgreSQL 12 每次重跑 ADD COLUMN IF NOT EXISTS 都会再加一份行内 CHECK；之前重跑留下的副本（name1、name2……）会被删掉。
ALTER TABLE community_skins ADD COLUMN IF NOT EXISTS category text NOT NULL DEFAULT 'other';
-- 社区键盘皮肤的来源：unknown、user 或 ai，取值与 Go 中的 skinOrigins 一致。发布时由服务端判定，见 community_skin_origin.go。PostgreSQL 11 起带常量默认值的加列不重写表；命名约束同样在下面的循环里添加。迁移只加列，已有作品都取默认值 unknown：没有照片的作品由服务启动后的后台回填（backfillSkinOrigins）分批改成 user，不放在这里，因为整份迁移在一个事务里、加列拿着表的 ACCESS EXCLUSIVE 锁直到提交，判断有没有照片又要把每一行的 design 从 TOAST 里取出来，作品多时会拖过启动的 30 秒期限，让旧副本对这张表的读写一直排队。
ALTER TABLE community_skins ADD COLUMN IF NOT EXISTS origin text NOT NULL DEFAULT 'unknown';
DO $$
DECLARE c record; d record;
BEGIN
 FOR c IN SELECT * FROM (VALUES
  ('community_skins','community_skins_category_check','category IN (''nature'',''guofeng'',''acg'',''cute'',''food'',''tech'',''minimal'',''other'')'),
  ('community_skins','community_skins_origin_check','origin IN (''unknown'',''user'',''ai'')')
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
-- 本服务生成过的皮肤插画的 64 位 dHash，发布时用来判定照片是否为 AI 插画。不关联账号，也不能还原出图片；长期保留，不随账号注销删除。
CREATE TABLE IF NOT EXISTS skin_artwork_fingerprints (
 hash bigint PRIMARY KEY,
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS community_skin_downloads (
 skin_id text NOT NULL REFERENCES community_skins(id) ON DELETE CASCADE,
 user_id text NOT NULL REFERENCES auth_users(id) ON DELETE CASCADE,
 PRIMARY KEY(skin_id,user_id)
);
-- 账号注销按 user_id 触发外键级联；主键以 skin_id 开头，不能避免扫描整张互动表。
CREATE INDEX IF NOT EXISTS community_skin_downloads_user ON community_skin_downloads(user_id);
CREATE TABLE IF NOT EXISTS community_skin_ratings (
 skin_id text NOT NULL REFERENCES community_skins(id) ON DELETE CASCADE,
 user_id text NOT NULL REFERENCES auth_users(id) ON DELETE CASCADE,
 stars integer NOT NULL CHECK(stars BETWEEN 1 AND 5),
 PRIMARY KEY(skin_id,user_id)
);
CREATE INDEX IF NOT EXISTS community_skin_ratings_user ON community_skin_ratings(user_id);
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
-- 我的资源、发布上限和后台作者作品查询都按 owner_id 筛选。
CREATE INDEX IF NOT EXISTS community_resources_owner ON community_resources(owner_id,created_at DESC);
-- 只保留资源 ID 和版本高水位，不关联作者；删除作品或注销账号不能让旧预览版本重新有效。
CREATE TABLE IF NOT EXISTS community_resource_revisions (
 id text PRIMARY KEY,
 revision integer NOT NULL CHECK(revision > 0)
);
-- 先锁住资源表再回填和安装触发器，避免迁移期间的删除遗漏最后版本。
LOCK TABLE community_resources IN SHARE ROW EXCLUSIVE MODE;
INSERT INTO community_resource_revisions AS history(id,revision)
 SELECT id,revision FROM community_resources
 ON CONFLICT(id) DO UPDATE SET revision=GREATEST(history.revision,EXCLUDED.revision);
CREATE OR REPLACE FUNCTION community_resource_revision() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='DELETE' THEN
  EXECUTE format('INSERT INTO %I.community_resource_revisions AS history(id,revision) VALUES($1,$2)
   ON CONFLICT(id) DO UPDATE SET revision=GREATEST(history.revision,EXCLUDED.revision)',TG_TABLE_SCHEMA)
   USING OLD.id,OLD.revision;
  RETURN OLD;
 END IF;
 -- 首次发布沿用默认版本；重建时以同一条记录的行锁串行分配，不能只读历史后再插入。
 EXECUTE format('INSERT INTO %I.community_resource_revisions AS history(id,revision) VALUES($1,$2)
  ON CONFLICT(id) DO UPDATE SET revision=GREATEST(history.revision+1,EXCLUDED.revision)
  RETURNING revision',TG_TABLE_SCHEMA) INTO NEW.revision USING NEW.id,NEW.revision;
 RETURN NEW;
END $$;
DO $$ BEGIN
 IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgrelid='community_resources'::regclass AND tgname='community_resource_revision') THEN
  CREATE TRIGGER community_resource_revision BEFORE INSERT OR DELETE ON community_resources FOR EACH ROW EXECUTE FUNCTION community_resource_revision();
 END IF;
END $$;
ALTER TABLE community_resources ENABLE TRIGGER community_resource_revision;
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
-- 资源收藏列表按用户筛选；主键的 resource_id 前缀无法支持 scope=saved。
CREATE INDEX IF NOT EXISTS community_resource_saves_user ON community_resource_saves(user_id,resource_id);
CREATE TABLE IF NOT EXISTS community_resource_ratings (
 resource_id text NOT NULL REFERENCES community_resources(id) ON DELETE CASCADE,
 user_id text NOT NULL REFERENCES auth_users(id) ON DELETE CASCADE,
 stars integer NOT NULL CHECK(stars BETWEEN 1 AND 5),
 PRIMARY KEY(resource_id,user_id)
);
CREATE INDEX IF NOT EXISTS community_resource_ratings_user ON community_resource_ratings(user_id);
-- 用户屏蔽的社区作者：屏蔽后该作者的作品不再出现在屏蔽者的社区目录里（mine 范围除外）。id 由服务端生成 UUID，响应只给出屏蔽记录的 ID 和作者显示名，不暴露被屏蔽用户的 ID。任意一方注销账号时由外键级联删除。
CREATE TABLE IF NOT EXISTS community_blocks (
 id text PRIMARY KEY,
 user_id text NOT NULL REFERENCES auth_users(id) ON DELETE CASCADE,
 blocked_id text NOT NULL REFERENCES auth_users(id) ON DELETE CASCADE,
 created_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(user_id,blocked_id)
);
-- UNIQUE(user_id,blocked_id) 服务目录的屏蔽探测和屏蔽者注销时的级联；被屏蔽者注销按 blocked_id 级联，需要单独的索引。
CREATE INDEX IF NOT EXISTS community_blocks_blocked ON community_blocks(blocked_id);

-- Curated candidate-window skin packages in the msime-skins format (skin.toml plus assets), served read-only through /v1/skins next to the embedded builtins and skins_root. Operators write them with scripts/candidate_skins_seed.py; clients never do. The service re-validates every manifest on read with the client loader's rules (internal/skins/client.go), so these constraints are a floor, not the whole contract.
CREATE TABLE IF NOT EXISTS candidate_skins (
 id text PRIMARY KEY CHECK(id ~ '^[a-z0-9][a-z0-9._-]{0,63}$' AND id NOT IN ('system','shuishan','light','paper','night','ink','custom','fluent','wechat','graphite','willow_green','autumn_osmanthus','microsoft','default')),
 manifest bytea NOT NULL CHECK(octet_length(manifest) BETWEEN 1 AND 65536),
 manifest_sha256 text GENERATED ALWAYS AS (encode(sha256(manifest),'hex')) STORED,
 published boolean NOT NULL DEFAULT true,
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now()
);
-- 保留 ID 与客户端的 is_reserved 一致：全局主题、msime-windows 的六个内置外观和它的 default 目录。旧库的列约束（PostgreSQL 命名为 candidate_skins_id_check）不含后加的 autumn_osmanthus、microsoft 与 default 时整体替换。以 NOT VALID 添加：已有行不回头校验，免得一行旧数据让启动迁移失败，新写入的行照常受约束；服务端读取时还会按客户端规则校验每个包。
DO $$
BEGIN
 IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid='candidate_skins'::regclass AND conname='candidate_skins_id_check' AND pg_get_constraintdef(oid) LIKE '%microsoft%') THEN
  ALTER TABLE candidate_skins DROP CONSTRAINT IF EXISTS candidate_skins_id_check;
  ALTER TABLE candidate_skins ADD CONSTRAINT candidate_skins_id_check CHECK(id ~ '^[a-z0-9][a-z0-9._-]{0,63}$' AND id NOT IN ('system','shuishan','light','paper','night','ink','custom','fluent','wechat','graphite','willow_green','autumn_osmanthus','microsoft','default')) NOT VALID;
 END IF;
END $$;
-- One row per package file other than skin.toml. Size and digest are generated from the bytes so they can never disagree with what is served; the media type follows from the extension.
CREATE TABLE IF NOT EXISTS candidate_skin_resources (
 skin_id text NOT NULL REFERENCES candidate_skins(id) ON DELETE CASCADE,
 path text NOT NULL CHECK(length(path) <= 256 AND path ~ '^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*$' AND path !~ '(^|/)\.\.?(/|$)' AND lower(path) ~ '\.(css|png|jpe?g|gif|webp|svg|ico|bmp|avif|woff2?|ttf|otf)$'),
 bytes bytea NOT NULL CHECK(octet_length(bytes) <= 4194304),
 size integer GENERATED ALWAYS AS (octet_length(bytes)) STORED,
 sha256 text GENERATED ALWAYS AS (encode(sha256(bytes),'hex')) STORED,
 PRIMARY KEY(skin_id,path)
);

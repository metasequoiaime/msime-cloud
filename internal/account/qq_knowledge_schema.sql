-- QQ 群答疑的向量索引：文档段落、issue 和 PR 各一行，embedding 是 chat 端点的 embedding 模型算出的向量，答疑时按余弦距离召回。需要 pgvector 扩展；没有安装时不建这两张表，答疑退回只用目录挑段落。
CREATE TABLE IF NOT EXISTS qq_knowledge_items (
 id text PRIMARY KEY,
 kind text NOT NULL CHECK (kind IN ('doc','issue','pr')),
 repo text NOT NULL DEFAULT '',
 number integer NOT NULL DEFAULT 0,
 title text NOT NULL,
 summary text NOT NULL,
 content_hash text NOT NULL,
 updated_at timestamptz NOT NULL DEFAULT now(),
 embedding halfvec(2048) NOT NULL
);
CREATE INDEX IF NOT EXISTS qq_knowledge_items_kind ON qq_knowledge_items(kind);
-- 每个仓库的增量同步进度：上一次同步到的 issue 更新时间。
CREATE TABLE IF NOT EXISTS qq_knowledge_cursors (
 repo text PRIMARY KEY,
 since timestamptz NOT NULL
);

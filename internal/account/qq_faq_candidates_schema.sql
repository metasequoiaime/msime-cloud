-- QQ 群答疑沉淀进文档的候选：机器人现查 GitHub 才答上的问题（answer 是当时的回答）和答不上的问题（answer 为空）。管理员发「文档缺口」看列表，「写进FAQ F编号」用「记录答案」的流程给知识库开 PR，「忽略 F编号」拿掉。state 是 open、done 或 ignored，ref 是写进 FAQ 时开的 PR。超过 30 天的由 Store.Prune 删除。
CREATE TABLE IF NOT EXISTS qq_faq_candidates (
 id bigserial PRIMARY KEY,
 question text NOT NULL,
 answer text NOT NULL DEFAULT '',
 state text NOT NULL DEFAULT 'open' CHECK (state IN ('open','done','ignored')),
 ref text NOT NULL DEFAULT '',
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS qq_faq_candidates_open ON qq_faq_candidates(created_at) WHERE state='open';

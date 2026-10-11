-- QQ 群答疑沉淀进文档的候选：机器人现查 GitHub 才答上的问题（answer 是当时的回答）和答不上的问题（answer 为空）。管理员发「文档缺口」看列表，「看 F编号」看全文，「写进FAQ F编号」用「记录答案」的流程给知识库开 PR，「忽略 F编号」拿掉。state 是 open、writing（正在写进 FAQ，claimed_at 是认领的时间）、done 或 ignored，ref 是写进 FAQ 时开的 PR。认领超过 5 分钟（account.QQFAQCandidateClaimTimeout）还在 writing 的当作写入被打断，可以重新认领。超过 30 天的由 Store.Prune 删除。
CREATE TABLE IF NOT EXISTS qq_faq_candidates (
 id bigserial PRIMARY KEY,
 question text NOT NULL,
 answer text NOT NULL DEFAULT '',
 state text NOT NULL DEFAULT 'open' CHECK (state IN ('open','writing','done','ignored')),
 ref text NOT NULL DEFAULT '',
 created_at timestamptz NOT NULL DEFAULT now(),
 claimed_at timestamptz
);
-- 已有的表补上认领时间，并把 state 的 CHECK 换成认 writing 的版本（约束名是 PostgreSQL 给列约束起的名字）；已经换过时不动。
ALTER TABLE qq_faq_candidates ADD COLUMN IF NOT EXISTS claimed_at timestamptz;
DO $$
BEGIN
 IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid='qq_faq_candidates'::regclass AND conname='qq_faq_candidates_state_check' AND pg_get_constraintdef(oid) LIKE '%writing%') THEN
  ALTER TABLE qq_faq_candidates DROP CONSTRAINT IF EXISTS qq_faq_candidates_state_check;
  ALTER TABLE qq_faq_candidates ADD CONSTRAINT qq_faq_candidates_state_check CHECK (state IN ('open','writing','done','ignored'));
 END IF;
END $$;
CREATE INDEX IF NOT EXISTS qq_faq_candidates_open ON qq_faq_candidates(created_at) WHERE state='open';

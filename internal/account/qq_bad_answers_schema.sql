-- QQ 群友说「不对」「答错了」的回答：question 和 answer 是被说答错的那一轮，feedback 是群友的原话，retry 是机器人重新查了以后的回答。管理员发「差评」看最近还没处理的（编号 B7 这样），「写进FAQ B编号」把重答写进 FAQ，「忽略 B编号」拿掉。state 是 open、done（写进了 FAQ）或 ignored。不存群友身份。超过 30 天的由 Store.Prune 删除。
CREATE TABLE IF NOT EXISTS qq_bad_answers (
 id bigserial PRIMARY KEY,
 question text NOT NULL,
 answer text NOT NULL,
 feedback text NOT NULL,
 retry text NOT NULL DEFAULT '',
 state text NOT NULL DEFAULT 'open' CHECK (state IN ('open','done','ignored')),
 created_at timestamptz NOT NULL DEFAULT now()
);
-- 已有的表补上 state 列，已有的差评都算还没处理。CHECK 用命名约束单独加：PostgreSQL 12 每次重跑 ADD COLUMN IF NOT EXISTS 都会再加一份行内 CHECK。
ALTER TABLE qq_bad_answers ADD COLUMN IF NOT EXISTS state text NOT NULL DEFAULT 'open';
DO $$
BEGIN
 IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid='qq_bad_answers'::regclass AND conname='qq_bad_answers_state_check') THEN
  ALTER TABLE qq_bad_answers ADD CONSTRAINT qq_bad_answers_state_check CHECK (state IN ('open','done','ignored'));
 END IF;
END $$;
CREATE INDEX IF NOT EXISTS qq_bad_answers_created ON qq_bad_answers(created_at);

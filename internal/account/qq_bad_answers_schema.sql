-- QQ 群友说「不对」「答错了」的回答：question 和 answer 是被说答错的那一轮，feedback 是群友的原话，retry 是机器人重新查了以后的回答。管理员发「差评」看最近的，用来改提示词和补文档。不存群友身份。超过 30 天的由 Store.Prune 删除。
CREATE TABLE IF NOT EXISTS qq_bad_answers (
 id bigserial PRIMARY KEY,
 question text NOT NULL,
 answer text NOT NULL,
 feedback text NOT NULL,
 retry text NOT NULL DEFAULT '',
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS qq_bad_answers_created ON qq_bad_answers(created_at);

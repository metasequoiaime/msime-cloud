-- QQ 群答疑最近几轮的问答，用来接上群友的追问。subject 是群和成员标识的摘要，不存原始 openid；只读最近 30 分钟，过期的由 Store.Prune 删除。
CREATE TABLE IF NOT EXISTS qq_conversations (
 id text PRIMARY KEY,
 subject text NOT NULL,
 question text NOT NULL,
 answer text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS qq_conversations_subject ON qq_conversations(subject, created_at);

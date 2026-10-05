-- 用户自愿贡献的语音样本：音频与识别文本，只给语音识别改进使用，保留 180 天由 Store.Prune 删除，注销账号时级联删除。
CREATE TABLE IF NOT EXISTS voice_contributions (
 id text PRIMARY KEY,
 user_id text NOT NULL REFERENCES auth_users(id) ON DELETE CASCADE,
 language text NOT NULL,
 provider text NOT NULL,
 duration_ms integer NOT NULL CHECK(duration_ms BETWEEN 1 AND 60000),
 transcript text NOT NULL,
 app_version text NOT NULL,
 audio_mime text NOT NULL CHECK(audio_mime IN ('audio/wav','audio/ogg')),
 audio bytea NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS voice_contributions_user ON voice_contributions(user_id);
CREATE INDEX IF NOT EXISTS voice_contributions_created ON voice_contributions(created_at);

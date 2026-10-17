-- 每台上传过的设备：平台和这台设备全部历史的候选位次计数（客户端只累计位次，没有按天的数）。每个用户最多 typingStatisticsDevices 台，新设备挤掉最久没上传的那台。
CREATE TABLE IF NOT EXISTS typing_statistics_devices (
 user_id text NOT NULL REFERENCES auth_users(id) ON DELETE CASCADE,
 device text NOT NULL,
 platform text NOT NULL DEFAULT '',
 ranks bigint[] NOT NULL DEFAULT '{}',
 beyond bigint NOT NULL DEFAULT 0,
 updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(user_id,device)
);

-- 用户在客户端打开「上传打字统计」后上传的每日汇总，只有计数：字数、活跃秒数、每小时的字数、各字符类别和各输入来源的字数，没有打出的内容。按设备分行，设备行被删掉时随外键一起删除，同一台设备重传同一天时整行替换；QQ 机器人的 /stats 把各设备相加。超过 typingStatisticsRetentionDays 天的由 Store.Prune 删除，注销账号时级联删除，用户关闭上传时客户端调用 DELETE /v1/users/me/typing-statistics 清空。
CREATE TABLE IF NOT EXISTS typing_statistics_days (
 user_id text NOT NULL REFERENCES auth_users(id) ON DELETE CASCADE,
 device text NOT NULL,
 day date NOT NULL,
 characters bigint NOT NULL,
 active_seconds bigint NOT NULL,
 hours bigint[],
 categories jsonb NOT NULL DEFAULT '{}',
 sources jsonb NOT NULL DEFAULT '{}',
 updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(user_id,device,day),
 FOREIGN KEY(user_id,device) REFERENCES typing_statistics_devices(user_id,device) ON DELETE CASCADE
);

-- QQ 群友和水杉账号的绑定。subject 是「群 openid/成员 openid」的 SHA-256，和 qq_drafts 一样不存原始 openid；同一个群友只绑一个账号，重新绑定覆盖旧的。
CREATE TABLE IF NOT EXISTS qq_account_links (
 id bigserial PRIMARY KEY,
 subject text NOT NULL UNIQUE,
 user_id text NOT NULL REFERENCES auth_users(id) ON DELETE CASCADE,
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS qq_account_links_user ON qq_account_links(user_id);

-- 机器人回给群友的一次性绑定码，只存 SHA-256。QQLinkCodeTTL 内有效，用过即删，同一个群友再要一次时旧码作废，过期的由 Store.Prune 删除。
CREATE TABLE IF NOT EXISTS qq_link_codes (
 code_hash text PRIMARY KEY,
 subject text NOT NULL,
 expires_at timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS qq_link_codes_subject ON qq_link_codes(subject);

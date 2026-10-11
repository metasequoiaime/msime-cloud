-- QQ 群友报的 bug 和需求整理成的 issue 草稿：机器人整理好后由管理员发「提交 D编号」提交。subject 是群和成员标识的摘要，不存原始 openid；report 是整理结果（JSON）；filed_at 非空表示已经认领提交（issue_ref 为空时正在提交），提交失败会清回空；认领超过 5 分钟（account.QQDraftClaimTimeout）还没写 issue_ref 的当作提交被打断，可以重新认领；issue_ref 为 'ignored'（account.QQDraftIgnored）表示管理员忽略了这份草稿，没有对应的 issue。超过 30 天的由 Store.Prune 删除。
CREATE TABLE IF NOT EXISTS qq_drafts (
 id bigserial PRIMARY KEY,
 subject text NOT NULL,
 original text NOT NULL,
 report text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 filed_at timestamptz,
 issue_ref text NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS qq_drafts_open ON qq_drafts(created_at) WHERE filed_at IS NULL;

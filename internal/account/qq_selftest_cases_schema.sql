-- QQ 群答疑自测从维护者核对过的答案里学到的题：管理员把答案写进 FAQ（记录答案、写进FAQ F编号、写进FAQ B编号）后，问题和答案记在这里，自测时连同固定的题一起跑，改提示词或换模型后同一个问题不会又答错。question 是写进 FAQ 的问题（模型改写过，和 FAQ PR 里的一样，不是群友原话），同一个问题只留最新的答案。超过 180 天的由 Store.Prune 删除。
CREATE TABLE IF NOT EXISTS qq_selftest_cases (
 question text PRIMARY KEY,
 answer text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now()
);

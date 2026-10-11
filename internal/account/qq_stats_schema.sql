-- QQ 群答疑每天每种处理结果的条数（处理结果见 server 里 handleQQFeedback 的 outcome），管理员「周报」用。day 按北京时间。超过 90 天的由 Store.Prune 删除。
CREATE TABLE IF NOT EXISTS qq_daily_stats (
 day date NOT NULL,
 outcome text NOT NULL,
 count integer NOT NULL DEFAULT 0,
 PRIMARY KEY (day, outcome)
);

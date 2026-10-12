-- QQ 群答疑的改进依据：答不上的问题（kind=unknown，topic 是模型的一句话概括，detail 写 docs 或 capability：没现查就答不上算文档没写，现查过还答不上算机器人能力问题）和自测结果（kind=selftest，detail 是通过数和没通过的题）。每周汇总成 msime-cloud 的一个 issue。不存群友原话和身份。超过 60 天的由 Store.Prune 删除。
CREATE TABLE IF NOT EXISTS qq_improvement_events (
 id bigserial PRIMARY KEY,
 kind text NOT NULL,
 topic text NOT NULL DEFAULT '',
 detail text NOT NULL DEFAULT '',
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS qq_improvement_events_created ON qq_improvement_events(created_at);

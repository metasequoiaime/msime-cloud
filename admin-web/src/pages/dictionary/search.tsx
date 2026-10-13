import { useEffect, useState } from "react";
import { useInfiniteQuery } from "@tanstack/react-query";
import { Search, X } from "lucide-react";
import { useAPI } from "../../api/client";
import { dictionarySearchSchema } from "../../api/dictionary";
import { keys } from "../../api/keys";
import { Button } from "../../ui/button";
import { Banner, Card } from "../../ui/card";
import { Empty, ErrorState } from "../../ui/states";
import { Pill } from "../../ui/pill";

const scopeLabels = { all: "全部词库（含上游来源）", custom: "自定义词库", packs: "专题词包", sources: "上游来源" } as const;
type Scope = keyof typeof scopeLabels;

export function DictionarySearch({ onOpen }: { onOpen: (path: string, query: string) => void }) {
  const api = useAPI();
  const [input, setInput] = useState("");
  const [scope, setScope] = useState<Scope>("all");
  const [active, setActive] = useState<{ query: string; scope: Scope }>();
  const [paused, setPaused] = useState(false);
  const results = useInfiniteQuery({
    queryKey: keys.page("dictionary", "search", active?.query ?? "", active?.scope ?? "all"),
    enabled: Boolean(active), initialPageParam: { cursor: 0, revision: "" },
    queryFn: ({ pageParam, signal }) => api.get(`dictionary/search?${new URLSearchParams({ q: active?.query ?? "", scope: active?.scope ?? "all", cursor: String(pageParam.cursor), revision: pageParam.revision })}`, dictionarySearchSchema, { signal }),
    getNextPageParam: last => last.has_more ? { cursor: last.next_cursor, revision: last.revision } : undefined,
    retry: false, staleTime: 60_000, refetchOnWindowFocus: false,
  });
  const { fetchNextPage, hasNextPage, isFetching, isError } = results;
  useEffect(() => { if (active && !paused && hasNextPage && !isFetching && !isError) void fetchNextPage(); }, [active, paused, hasNextPage, isFetching, isError, fetchNextPage]);
  const pages = active ? results.data?.pages ?? [] : [];
  const last = pages.at(-1);
  const files = pages.flatMap(page => page.files);
  const skipped = pages.flatMap(page => page.skipped);
  const matches = files.reduce((sum, file) => sum + file.total, 0);
  const complete = Boolean(last && !last.has_more && !results.isFetching);
  return <Card className="mb-3.5">
    <form aria-label="跨词库词条搜索" onSubmit={event => { event.preventDefault(); if (input.trim()) { setActive({ query: input.trim(), scope }); setPaused(false); } }} className="flex flex-wrap items-center gap-2">
      <label className="flex h-11 min-w-0 flex-[2_1_240px] items-center gap-2 rounded-xl bg-panel-2 px-3 text-muted"><Search size={18} aria-hidden="true" /><input value={input} onChange={event => setInput(event.target.value)} maxLength={100} aria-label="搜索所有词库词条" placeholder="搜索词语、拼音、编码或译文" className="min-w-0 flex-1 bg-transparent text-[14px] text-ink outline-none placeholder:text-muted" /></label>
      <select value={scope} onChange={event => setScope(event.target.value as Scope)} aria-label="词条搜索范围" className="h-11 min-w-0 rounded-xl bg-panel-2 px-3 text-[13px] text-ink">{Object.entries(scopeLabels).map(([key, label]) => <option key={key} value={key}>{label}</option>)}</select>
      <Button type="submit" variant="primary" disabled={!input.trim()}><Search size={15} aria-hidden="true" />搜索</Button>
      {active && <Button variant="ghost" aria-label="关闭跨词库搜索" onClick={() => { setActive(undefined); setInput(""); }}><X size={15} aria-hidden="true" />清空</Button>}
    </form>
    {!active ? <p className="mb-0 mt-2 text-[12px] text-muted">不用先选文件，直接在所有词库中查找词条，搜索结果可打开继续编辑。</p> : <>
      <div className="mt-3 flex flex-wrap items-center justify-between gap-2"><p aria-live="polite" className="m-0 text-[13px] text-body">{complete ? "搜索完成" : paused ? "已暂停搜索" : results.isError ? "搜索中断" : "正在搜索"} · 「{active.query}」 · 找到 {matches.toLocaleString("zh-CN")} 条 · {last?.next_cursor ?? 0} / {last?.total_files ?? "—"} 个文件</p>
        {!complete && !results.isError && <Button size="sm" variant="outline" onClick={() => setPaused(value => !value)}>{paused ? "继续搜索" : "暂停搜索"}</Button>}
      </div>
      {results.isError && <div className="mt-3"><ErrorState error={results.error} onRetry={() => { setPaused(false); if (pages.length) void results.fetchNextPage(); else void results.refetch(); }} /></div>}
      {last?.truncated && <Banner tone="warn" className="mt-3">仓库目录超过 GitHub 返回上限，搜索未覆盖完整目录。</Banner>}
      {skipped.length > 0 && <details className="mt-3 text-[12px] text-muted"><summary>有 {skipped.length} 个文件未能搜索</summary><ul>{skipped.map(file => <li key={file.path}>{file.path}：{file.reason}</li>)}</ul></details>}
      <div className="mt-3 grid max-h-[380px] gap-3 overflow-y-auto">{files.map(file => <div key={file.path} className="rounded-xl bg-panel-2 p-3">
        <div className="flex flex-wrap items-center justify-between gap-2"><span className="min-w-0 break-all text-[13px] font-semibold text-ink">{file.path} <Pill tone={file.editable ? "ok" : "mute"}>{file.editable ? "可维护" : "只读"}</Pill></span><Button size="sm" variant="outline" onClick={() => onOpen(file.path, active.query)}>{file.editable ? "打开并编辑" : "打开查看"}</Button></div>
        <p className="mb-2 mt-1 text-[12px] text-muted">匹配 {file.total.toLocaleString("zh-CN")} 条{file.rows.length > 5 ? "，预览前 5 条，打开文件查看全部" : ""}</p>
        <ul className="m-0 grid list-none gap-1 p-0">{file.rows.slice(0, 5).map(row => <li key={row.line}><button type="button" onClick={() => onOpen(file.path, active.query)} className="flex w-full gap-3 rounded-lg px-2 py-1 text-left text-[12.5px] text-body hover:bg-accent-soft"><span className="shrink-0 text-muted">{row.line}</span><span className="min-w-0 break-all">{row.columns.join(" · ")}</span></button></li>)}</ul>
      </div>)}</div>
      {complete && !files.length && <Empty title={`未找到「${active.query}」`}>试试更短的词语、拼音或编码。</Empty>}
    </>}
  </Card>;
}

import { useEffect, useMemo, useRef, useState } from "react";
import { useMutation, useQuery } from "@tanstack/react-query";
import { useBlocker } from "@tanstack/react-router";
import { ExternalLink, Maximize2, Minimize2, RefreshCw, Search, X } from "lucide-react";
import { APIError, useAPI } from "../../api/client";
import { columnLabels, dictionaryChangeSchema, dictionaryContentSchema } from "../../api/dictionary";
import { keys } from "../../api/keys";
import { noPermissionHint, usePermissions } from "../../shell/permissions";
import { Button, buttonVariants } from "../../ui/button";
import { Banner, Card, CardHeader } from "../../ui/card";
import { ErrorState, SkeletonRows } from "../../ui/states";
import { Pill } from "../../ui/pill";
import { DictionarySheet } from "./sheet";
import type { DictionarySheetHandle } from "./sheet";
import { draftError, emptyHistory, redoHistory, undoHistory, updateHistory } from "./sheet-model";
import type { Draft } from "./sheet-model";

const changeLabels = { add: "新增", replace: "修改", delete: "删除" } as const;

export function FileDetail({ path, initialQuery = "", onDirty, focused, onToggleFocus }: { path: string; initialQuery?: string; onDirty: (dirty: boolean) => void; focused: boolean; onToggleFocus: () => void }) {
  const api = useAPI();
  const { can } = usePermissions();
  const permitted = can("review_dict_pr");
  const custom = path.startsWith("custom/");
  const translation = path.endsWith("translations.txt");
  const [page, setPage] = useState(1);
  const [search, setSearch] = useState(initialQuery);
  const [query, setQuery] = useState(initialQuery);
  const [history, setHistory] = useState(emptyHistory);
  const drafts = history.present;
  const [sha, setSHA] = useState("");
  const [editing, setEditing] = useState(false);
  const sheet = useRef<DictionarySheetHandle>(null);
  const [saveError, setSaveError] = useState("");
  const [uncertain, setUncertain] = useState(false);
  useEffect(() => { if (editing || search.trim() === query) return; const timer = window.setTimeout(() => { setQuery(search.trim()); setPage(1); }, 350); return () => window.clearTimeout(timer); }, [search, editing, query]);
  const dirty = drafts.length > 0 || editing;
  useEffect(() => { onDirty(dirty); }, [dirty, onDirty]);
  const blocker = useBlocker({ shouldBlockFn: ({ current, next }) => dirty && current.pathname !== next.pathname, enableBeforeUnload: dirty, withResolver: true });
  const content = useQuery({
    queryKey: keys.page("dictionary", "file", path, page, query),
    queryFn: ({ signal }) => api.get(`dictionary/file?${new URLSearchParams({ path, page: String(page), q: query })}`, dictionaryContentSchema, { signal }),
    staleTime: 60_000, refetchOnWindowFocus: false,
  });
  const data = content.data;
  useEffect(() => { if (editing && data) setSHA(current => current || data.sha); }, [editing, data]);
  const editable = Boolean(data?.editable) && permitted;
  const labels = useMemo(() => columnLabels(path, Math.max(0, ...(data?.rows.map(row => row.columns.length) ?? []))), [path, data?.rows]);
  const defaults = useMemo(() => labels.map((_, index) => index === 2 ? data?.rows[0]?.columns[2] ?? "1" : ""), [labels, data?.rows]);
  const changed = Boolean(sha && data && sha !== data.sha);
  const invalid = drafts.filter(draft => draftError(path, draft));
  const mutation = useMutation({
    mutationFn: (changes: Draft[]) => api.post("dictionary/changes", dictionaryChangeSchema, { path, sha: sha || data?.sha, changes: changes.map(({ operation, line, columns }) => ({ operation, line, columns })) }),
    onSuccess: () => { setHistory(emptyHistory); setSHA(""); setSaveError(""); },
    onError: error => { if (error instanceof APIError && ["dictionary_uncertain", "dictionary_not_recorded"].includes(error.code)) setUncertain(true); },
  });
  const conflict = changed || (mutation.error instanceof APIError && mutation.error.code === "dictionary_changed");
  const locked = uncertain || conflict || mutation.isPending;
  const change = (next: Draft[]) => { if (!sha && data) setSHA(data.sha); setHistory(current => updateHistory(current, next)); setSaveError(""); mutation.reset(); };
  const save = (next: Draft[]) => {
    if (!editable || locked || !next.length) return;
    const invalidRows = next.filter(draft => draftError(path, draft));
    if (invalidRows.length) { setSaveError(`${invalidRows.length} 行尚未填写完整或权重无效，请修正后保存。`); return; }
    mutation.mutate(next);
  };
  const changeView = (nextPage: number, nextQuery = query) => {
    // 搜索和翻页会重建表格，当前输入校验失败时必须留在原视图。
    if (sheet.current?.commit() === null) return false;
    setPage(nextPage); setQuery(nextQuery); return true;
  };
  const discard = () => { sheet.current?.discard(); setHistory(emptyHistory); setSHA(""); setUncertain(false); setSaveError(""); mutation.reset(); };
  const pullURL = mutation.data?.url ?? (mutation.error instanceof APIError && typeof mutation.error.details.pull_url === "string" ? mutation.error.details.pull_url : undefined);
  return <Card>
    <CardHeader className="sticky top-0 z-30 -mx-2 bg-panel px-2 py-2" title={path.split("/").pop()} sub={<span className="break-all font-mono">{path}</span>} actions={<>
      <Button size="sm" variant="outline" aria-pressed={focused} onClick={onToggleFocus}>{focused ? <Minimize2 size={14} aria-hidden="true" /> : <Maximize2 size={14} aria-hidden="true" />}{focused ? "退出专注" : "专注编辑"}</Button>
      <Button size="sm" variant="outline" disabled={mutation.isPending || content.isFetching || editing} onClick={() => void content.refetch()}><RefreshCw size={14} aria-hidden="true" />刷新</Button>
      {data?.url && <a href={data.url} target="_blank" rel="noreferrer" className={buttonVariants({ size: "sm", variant: "outline" })}><ExternalLink size={14} aria-hidden="true" />源文件</a>}
      {editable && <Button size="sm" variant="primary" disabled={locked || (!drafts.length && !editing) || (Boolean(invalid.length) && !editing)} onPointerDown={event => event.preventDefault()} onClick={() => { const next = sheet.current?.commit(); if (next) save(next); }}>{mutation.isPending ? "正在保存…" : `保存变更${drafts.length ? `（${drafts.length}）` : ""}`}</Button>}
    </>} />
    <form aria-label="当前词库词条搜索" onSubmit={event => { event.preventDefault(); changeView(1, search.trim()); }} className="mb-3 flex flex-wrap items-center gap-2 rounded-xl bg-panel-2 p-2 text-muted">
      <Search size={16} aria-hidden="true" /><input value={search} maxLength={100} onChange={event => setSearch(event.target.value)} placeholder="搜索当前词库中的词条、编码或译文" aria-label="搜索当前文件词条" className="h-9 min-w-0 flex-1 bg-transparent text-[13px] text-ink outline-none placeholder:text-muted" />
      {search && <Button size="icon" variant="ghost" aria-label="清空词条搜索" onPointerDown={event => event.preventDefault()} onClick={() => { if (changeView(1, "")) setSearch(""); }}><X size={14} aria-hidden="true" /></Button>}<Button type="submit" variant="outline" onPointerDown={event => event.preventDefault()}>搜索词条</Button>
    </form>
    {blocker.status === "blocked" && <Banner tone="warn" className="mb-3"><div className="flex flex-wrap items-center justify-between gap-2"><span>离开页面会放弃未提交的词条变更。</span><div className="flex gap-2"><Button size="sm" variant="outline" onClick={() => blocker.reset()}>留在此页</Button><Button size="sm" variant="danger-outline" onClick={() => blocker.proceed()}>放弃并离开</Button></div></div></Banner>}
    {mutation.isSuccess && <Banner tone="ok" className="mb-3">已创建 PR #{mutation.data.number}。<a href={mutation.data.url} target="_blank" rel="noreferrer">查看并审核变更</a>，合入后生效。</Banner>}
    {mutation.isError && <div className="mb-3"><ErrorState error={mutation.error} />{pullURL && <a href={pullURL} target="_blank" rel="noreferrer">查看已创建的 PR</a>}{uncertain && <p className="text-xs text-muted">提交已停止，请先检查仓库的 Pull Request 与 dictionary-admin 分支，再决定是否重新编辑。</p>}</div>}
    {conflict && <Banner tone="warn" className="mb-3">文件已更新，当前草稿无法提交。请先记录待提交的内容，再放弃草稿并基于新版本编辑。</Banner>}
    {data && <p className="mb-3 text-[12px] text-muted">{!data.editable ? "只读词库支持选择、复制和搜索。" : !permitted ? noPermissionHint : custom ? translation ? "译文直接编辑，保存时追加修正记录。" : "已有词条只读；在表格末尾直接录入，或复制已有词条后补词。" : "直接输入覆盖单元格，双击或 F2 修改内容；切换单元格自动暂存。"}</p>}
    {saveError && <p role="alert" className="mb-3 text-[13px] text-bad">{saveError}</p>}
    {content.isPending ? <SkeletonRows rows={8} /> : content.isError ? <ErrorState error={content.error} onRetry={() => void content.refetch()} /> : data ? <>
      <div className="mb-3 flex flex-wrap justify-between gap-2 text-[12px] text-muted"><span aria-live="polite">{content.isFetching ? "正在搜索…" : query ? `找到 ${data.total.toLocaleString("zh-CN")} 条「${query}」的匹配` : `共 ${data.total.toLocaleString("zh-CN")} 条词条`} · {data.encoding}</span><span>{data.editable ? custom ? "只追加" : "可编辑" : "只读"} · <span className="font-mono">{data.sha.slice(0, 8)}</span></span></div>
      {query && data.total === 0 && <p className="mb-3 text-[13px] text-muted">未找到「{query}」</p>}
      <DictionarySheet ref={sheet} key={`${page}:${query}`} path={path} originals={data.rows} labels={labels} defaults={defaults} drafts={drafts} editable={editable} locked={locked} hasMore={data.has_more} focused={focused}
        canUndo={Boolean(history.past.length)} canRedo={Boolean(history.future.length)} onChange={change} onEditing={setEditing}
        onUndo={() => { setHistory(undoHistory); mutation.reset(); }} onRedo={() => { setHistory(redoHistory); mutation.reset(); }} onSave={save} />
      <div className="mt-3 flex flex-wrap items-center justify-between gap-2 text-[12px] text-muted"><span>第 {page} / {Math.max(1, Math.ceil(data.total / 100))} 页 · 每页 100 条</span><div className="flex gap-2"><Button size="sm" variant="outline" disabled={page <= 1 || mutation.isPending} onPointerDown={event => event.preventDefault()} onClick={() => changeView(page - 1)}>上一页</Button><Button size="sm" variant="outline" disabled={!data.has_more || mutation.isPending} onPointerDown={event => event.preventDefault()} onClick={() => changeView(page + 1)}>下一页</Button></div></div>
    </> : null}
    {drafts.length > 0 && <details className="mt-3 rounded-xl bg-panel-2 p-3 text-[12.5px] text-body"><summary className="cursor-pointer font-medium">查看全部 {drafts.length} 项变更{invalid.length ? ` · ${invalid.length} 行需修正` : ""}</summary><ul className="mb-0 mt-2 grid list-none gap-2 p-0">{drafts.map(draft => <li key={draft.id} className="flex items-center gap-2"><Pill tone={draftError(path, draft) ? "bad" : draft.operation === "delete" ? "bad" : "ok"}>{changeLabels[draft.operation]}</Pill><span className="min-w-0 flex-1 break-all">{draft.line || draft.sourceLine ? `第 ${draft.sourceLine ?? draft.line} 行 · ` : ""}{draft.columns.join(" · ")}{draftError(path, draft) ? `（${draftError(path, draft)}）` : ""}</span><Button size="sm" variant="ghost" disabled={locked} onClick={() => change(drafts.filter(value => value.id !== draft.id))}>撤销此行</Button></li>)}</ul></details>}
    {/* 按钮直接处理输入缓冲，保留焦点以免失焦校验插入提示后移走点击目标。 */}
    {dirty && <div className="mt-3 flex flex-wrap items-center justify-between gap-2 text-[12px] text-muted"><span aria-live="polite">待保存变更 · {drafts.length} 项{invalid.length ? ` · ${invalid.length} 行需修正` : ""} · 保存为 PR，合入后生效</span><Button size="sm" variant="ghost" disabled={mutation.isPending} onPointerDown={event => event.preventDefault()} onClick={discard}>放弃草稿</Button></div>}
  </Card>;
}

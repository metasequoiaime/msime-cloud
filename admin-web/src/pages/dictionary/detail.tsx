import { useEffect, useMemo, useState } from "react";
import { useMutation, useQuery } from "@tanstack/react-query";
import { useBlocker } from "@tanstack/react-router";
import { Check, ExternalLink, Pencil, Plus, RefreshCw, Search, X } from "lucide-react";
import { APIError, useAPI } from "../../api/client";
import { columnLabels, dictionaryChangeSchema, dictionaryContentSchema } from "../../api/dictionary";
import type { DictionaryEdit, DictionaryRow } from "../../api/dictionary";
import { keys } from "../../api/keys";
import { noPermissionHint, usePermissions } from "../../shell/permissions";
import { Button, buttonVariants } from "../../ui/button";
import { Banner, Card, CardHeader } from "../../ui/card";
import { Empty, ErrorState, SkeletonRows } from "../../ui/states";
import { cn } from "../../ui/cn";
import { Pill } from "../../ui/pill";

const inputClass = "h-10 w-full rounded-[10px] bg-panel-2 px-3 text-[13px] text-ink outline-none focus:ring-[1.5px] focus:ring-accent";
const changeLabels = { add: "新增", replace: "修改", delete: "删除" } as const;
type Draft = DictionaryEdit & { id: string; sourceLine?: number };
type Editor = Draft & { operation: "add" | "replace"; focus: number };

export function FileDetail({ path, initialQuery = "", onDirty }: { path: string; initialQuery?: string; onDirty: (dirty: boolean) => void }) {
  const api = useAPI();
  const { can } = usePermissions();
  const permitted = can("review_dict_pr");
  const custom = path.startsWith("custom/");
  const translation = path.endsWith("translations.txt");
  const [page, setPage] = useState(1);
  const [search, setSearch] = useState(initialQuery);
  const [query, setQuery] = useState(initialQuery);
  const [drafts, setDrafts] = useState<Draft[]>([]);
  const [sha, setSHA] = useState("");
  const [editor, setEditor] = useState<Editor>();
  const [editorError, setEditorError] = useState("");
  const [uncertain, setUncertain] = useState(false);
  const editing = Boolean(editor);
  useEffect(() => { if (editing || search.trim() === query) return; const timer = window.setTimeout(() => { setQuery(search.trim()); setPage(1); }, 350); return () => window.clearTimeout(timer); }, [search, editing, query]);
  const dirty = drafts.length > 0 || Boolean(editor);
  useEffect(() => { onDirty(dirty); if (!dirty) setSHA(""); }, [dirty, onDirty]);
  const blocker = useBlocker({ shouldBlockFn: ({ current, next }) => dirty && current.pathname !== next.pathname, enableBeforeUnload: dirty, withResolver: true });
  const content = useQuery({
    queryKey: keys.page("dictionary", "file", path, page, query),
    queryFn: ({ signal }) => api.get(`dictionary/file?${new URLSearchParams({ path, page: String(page), q: query })}`, dictionaryContentSchema, { signal }),
    staleTime: 60_000, refetchOnWindowFocus: false,
  });
  const data = content.data;
  const editable = Boolean(data?.editable) && permitted;
  const labels = useMemo(() => columnLabels(path, Math.max(0, ...(data?.rows.map(row => row.columns.length) ?? []))), [path, data?.rows]);
  const editLabels = columnLabels(path, path.endsWith("translations.txt") ? 2 : 3);
  const changed = Boolean(sha && data && sha !== data.sha);
  const mutation = useMutation({
    mutationFn: () => api.post("dictionary/changes", dictionaryChangeSchema, { path, sha, changes: drafts.map(({ operation, line, columns }) => ({ operation, line, columns })) }),
    onSuccess: () => { setDrafts([]); setSHA(""); setEditor(undefined); },
    onError: error => { if (error instanceof APIError && ["dictionary_uncertain", "dictionary_not_recorded"].includes(error.code)) setUncertain(true); },
  });
  const pin = () => { if (!sha && data) setSHA(data.sha); mutation.reset(); };
  const locked = uncertain || changed || mutation.isPending;
  const rowDraft = (row: DictionaryRow) => drafts.find(draft => (draft.sourceLine ?? draft.line) === row.line);
  const openEditor = (row?: DictionaryRow, focus = 0, draft?: Draft) => {
    if (editor || locked) return;
    pin(); setEditorError("");
    const current = draft ?? (row && rowDraft(row));
    const append = !row || custom;
    const columns = current?.operation !== "delete" && current?.columns ? current.columns : row?.columns ?? editLabels.map((_, index) => index === 2 ? data?.rows[0]?.columns[2] ?? "1" : "");
    setEditor({ id: current?.id ?? (row && (!custom || translation) ? `line-${row.line}` : crypto.randomUUID()), operation: append ? "add" : "replace", line: append ? 0 : row.line, sourceLine: custom && translation ? row?.line : undefined, columns: [...columns], focus });
  };
  const stage = () => {
    if (!editor || locked) return;
    const columns = editor.columns.map(value => value.trim());
    if (columns.length !== editLabels.length || columns.some(value => !value || /[\t\r\n]/.test(value))) { setEditorError("请填写每个字段，字段内不能含换行或制表符。"); return; }
    if (!translation && (!/^\d+$/.test(columns[2]) || Number(columns[2]) < 1 || Number(columns[2]) > (path.endsWith("english.txt") && !custom ? 10 : 10000))) { setEditorError("请填写有效的正整数权重。中文词条最多 10000，专题英文词条最多 10。"); return; }
    if (drafts.length >= 100 && !drafts.some(draft => draft.id === editor.id)) { setEditorError("一次最多保存 100 项变更，请先保存当前变更。"); return; }
    const original = data?.rows.find(row => row.line === (editor.sourceLine ?? editor.line));
    if (original && columns.every((value, index) => value === original.columns[index])) {
      setDrafts(current => current.filter(draft => draft.id !== editor.id));
    } else {
      const { focus: _, ...entry } = editor;
      setDrafts(current => [...current.filter(draft => draft.id !== entry.id), { ...entry, columns }]);
    }
    setEditor(undefined); setEditorError("");
  };
  const cancelEditor = () => { setEditor(undefined); setEditorError(""); };
  const editingCells = (entry: Editor) => <>
    {editLabels.map((label, index) => <td key={label} className="min-w-32 px-2 py-2">
      <input ref={element => { if (element && entry.focus === index) element.focus(); }} value={entry.columns[index] ?? ""} aria-label={label} maxLength={300}
        readOnly={custom && translation && Boolean(entry.sourceLine) && index === 0}
        inputMode={index === 2 ? "numeric" : "text"} className={cn(inputClass, "min-w-28 read-only:text-muted")}
        onKeyDown={event => { if (event.nativeEvent.isComposing) return; if (event.key === "Enter") { event.preventDefault(); stage(); } if (event.key === "Escape") { event.preventDefault(); cancelEditor(); } }}
        onChange={event => { const next = [...entry.columns]; next[index] = event.target.value; setEditor({ ...entry, columns: next, focus: index }); }} />
    </td>)}
    <td className="px-3 py-2"><div className="flex gap-1"><Button size="sm" variant="primary" onClick={stage}><Check size={13} aria-hidden="true" />确认</Button><Button size="sm" variant="ghost" onClick={cancelEditor}>取消</Button></div></td>
  </>;
  const removeRow = (row: DictionaryRow) => { pin(); setDrafts(current => [...current.filter(draft => draft.line !== row.line), { id: `line-${row.line}`, operation: "delete", line: row.line, columns: [] }]); };
  const discard = () => { setDrafts([]); setSHA(""); setEditor(undefined); setUncertain(false); mutation.reset(); };
  const pullURL = mutation.data?.url ?? (mutation.error instanceof APIError && typeof mutation.error.details.pull_url === "string" ? mutation.error.details.pull_url : undefined);
  return <Card>
    <CardHeader title={path.split("/").pop()} sub={<span className="break-all font-mono">{path}</span>} actions={<>
      <Button size="sm" variant="outline" disabled={mutation.isPending || content.isFetching} onClick={() => void content.refetch()}><RefreshCw size={14} aria-hidden="true" />刷新</Button>
      {data?.url && <a href={data.url} target="_blank" rel="noreferrer" className={buttonVariants({ size: "sm", variant: "outline" })}><ExternalLink size={14} aria-hidden="true" />源文件</a>}
      {data?.editable && <Button size="sm" variant="primary" disabled={!permitted || uncertain || mutation.isPending || changed || drafts.length >= 100 || Boolean(editor)} title={permitted ? undefined : noPermissionHint} onClick={() => openEditor()}><Plus size={14} aria-hidden="true" />新增词条</Button>}
    </>} />
    <form aria-label="当前词库词条搜索" onSubmit={event => { event.preventDefault(); if (editing) return; setQuery(search.trim()); setPage(1); }} className="mb-3 flex flex-wrap items-center gap-2 rounded-xl bg-panel-2 p-2 text-muted">
      <Search size={16} aria-hidden="true" /><input disabled={editing} value={search} maxLength={100} onChange={event => setSearch(event.target.value)} placeholder="搜索当前词库中的词条、编码或译文" aria-label="搜索当前文件词条" className="h-9 min-w-0 flex-1 bg-transparent text-[13px] text-ink outline-none placeholder:text-muted" />
      {search && <Button size="icon" variant="ghost" aria-label="清空词条搜索" disabled={editing} onClick={() => { setSearch(""); setQuery(""); setPage(1); }}><X size={14} aria-hidden="true" /></Button>}<Button type="submit" variant="outline" disabled={editing}>搜索词条</Button>
    </form>
    {blocker.status === "blocked" && <Banner tone="warn" className="mb-3"><div className="flex flex-wrap items-center justify-between gap-2"><span>离开页面会放弃未提交的词条变更。</span><div className="flex gap-2"><Button size="sm" variant="outline" onClick={() => blocker.reset()}>留在此页</Button><Button size="sm" variant="danger-outline" onClick={() => blocker.proceed()}>放弃并离开</Button></div></div></Banner>}
    {mutation.isSuccess && <Banner tone="ok" className="mb-3">已创建 PR #{mutation.data.number}。<a href={mutation.data.url} target="_blank" rel="noreferrer">查看并审核变更</a>，合入后生效。</Banner>}
    {mutation.isError && <div className="mb-3"><ErrorState error={mutation.error} />{pullURL && <a href={pullURL} target="_blank" rel="noreferrer">查看已创建的 PR</a>}{uncertain && <p className="text-xs text-muted">提交已停止，请先检查仓库的 Pull Request 与 dictionary-admin 分支，再决定是否重新编辑。</p>}</div>}
    {changed && <Banner tone="warn" className="mb-3">文件已更新，当前草稿无法提交。请先记录待提交的内容，再放弃草稿并基于新版本编辑。</Banner>}
    {data && <div className="mb-3 rounded-xl bg-panel-2 px-3 py-2 text-[12.5px] text-muted">{!data.editable ? "这个文件只能搜索和查看。请选择可维护的自定义词库或专题词包进行编辑。" : !permitted ? noPermissionHint : custom ? translation ? "点击译文即可修改；保存时追加新的译文，保留原始记录。" : "可新增词条或复制已有词条后补词；已有记录按仓库约定保留。修改和删除已有词条请使用专题词包。" : "点击词条的编辑按钮或双击字段直接修改，Enter 确认，Esc 取消。"}</div>}
    {editorError && <p role="alert" className="mb-3 text-[13px] text-bad">{editorError}</p>}
    {content.isPending ? <SkeletonRows rows={8} /> : content.isError ? <ErrorState error={content.error} onRetry={() => void content.refetch()} /> : data ? <>
      <div className="mb-3 flex flex-wrap justify-between gap-2 text-[12px] text-muted"><span aria-live="polite">{content.isFetching ? "正在搜索…" : query ? `找到 ${data.total.toLocaleString("zh-CN")} 条「${query}」的匹配` : `共 ${data.total.toLocaleString("zh-CN")} 条词条`} · {data.encoding}</span><span>{data.editable ? custom ? "只追加 · 翻译可用新行覆盖" : "词包支持增删改" : "上游来源 · 只读"} · <span className="font-mono">{data.sha.slice(0, 8)}</span></span></div>
      <div className="max-h-[620px] overflow-auto rounded-xl ring-1 ring-hair" aria-busy={content.isFetching}><table aria-label="词条编辑表格" className="w-full min-w-[520px] border-collapse text-left text-[12.5px]">
        <thead className="sticky top-0 z-10 bg-panel-2 text-muted"><tr><th className="w-14 px-3 py-2 font-medium">行</th>{labels.map(label => <th key={label} className="px-3 py-2 font-medium">{label}</th>)}{editable && <th className="px-3 py-2 font-medium">操作</th>}</tr></thead>
        <tbody>
          {editor && editor.line === 0 && !editor.sourceLine && <tr className="bg-accent-soft"><td className="px-3 text-accent-ink">新增</td>{editingCells(editor)}</tr>}
          {drafts.filter(draft => draft.operation === "add" && !draft.sourceLine && draft.id !== editor?.id).map(draft => <tr key={draft.id} className="border-t border-hair bg-accent-soft"><td className="px-3 text-accent-ink">新增</td>{draft.columns.map((value, index) => <td key={editLabels[index]} className="break-words px-3 py-2 text-body">{value}</td>)}<td className="px-3 py-2"><div className="flex gap-1"><Button size="sm" variant="outline" disabled={locked || Boolean(editor)} onClick={() => openEditor(undefined, 0, draft)}>编辑</Button><Button size="sm" variant="ghost" disabled={mutation.isPending} onClick={() => setDrafts(current => current.filter(value => value.id !== draft.id))}>撤销</Button></div></td></tr>)}
          {data.rows.map(row => {
            const staged = rowDraft(row);
            const activeEditor = editor && (editor.sourceLine ?? editor.line) === row.line;
            return <tr key={row.line} className={cn("border-t border-hair align-top hover:bg-panel-2", (activeEditor || staged) && "bg-accent-soft")}><td className="px-3 py-2 font-mono text-muted">{row.line}</td>
              {activeEditor ? editingCells(editor) : <>{labels.map((label, index) => <td key={label} onDoubleClick={() => { if (editable && !custom) openEditor(row, index); else if (editable && translation && index === 1) openEditor(row, index); }} className="max-w-[300px] break-words px-3 py-2 whitespace-pre-wrap text-body">
                {staged?.operation === "delete" ? <del>{row.columns[index]}</del> : custom && translation && index === 1 && editable ? <button type="button" disabled={locked || Boolean(editor)} onClick={() => openEditor(row, index)} className="text-left underline decoration-dotted decoration-accent underline-offset-4">{(staged?.columns ?? row.columns)[index]}</button> : (staged?.columns ?? row.columns)[index] ?? ""}
              </td>)}{editable && <td className="px-3 py-2"><div className="flex gap-1">
                {staged ? <><Pill tone={staged.operation === "delete" ? "bad" : "ok"}>{changeLabels[staged.operation]}</Pill><Button size="sm" variant="ghost" disabled={mutation.isPending || Boolean(editor)} onClick={() => setDrafts(current => current.filter(value => value.id !== staged.id))}>撤销</Button></> : null}
                {staged?.operation !== "delete" && <Button size="sm" variant="outline" disabled={locked || Boolean(editor) || drafts.length >= 100} onClick={() => openEditor(row, custom && translation ? 1 : 0)}><Pencil size={12} aria-hidden="true" />{custom ? translation ? "修改译文" : "复制新增" : "编辑"}</Button>}
                {!custom && staged?.operation !== "delete" && <Button size="sm" variant="danger-outline" disabled={locked || Boolean(editor) || drafts.length >= 100} onClick={() => removeRow(row)}>删除</Button>}
              </div></td>}</>}
            </tr>;
          })}
        </tbody>
      </table>{data.rows.length === 0 && !editor && <Empty title={query ? `未找到「${query}」` : "没有词条"}>{query ? "试试词语的一部分、拼音、编码或译文，也可清空搜索查看全部词条。" : "点击新增词条开始编辑。"}</Empty>}</div>
      <div className="mt-3 flex flex-wrap items-center justify-between gap-2 text-[12px] text-muted"><span>第 {page} / {Math.max(1, Math.ceil(data.total / 100))} 页 · 每页 100 条</span><div className="flex gap-2"><Button size="sm" variant="outline" disabled={page <= 1 || mutation.isPending || editing} onClick={() => setPage(value => value - 1)}>上一页</Button><Button size="sm" variant="outline" disabled={!data.has_more || mutation.isPending || editing} onClick={() => setPage(value => value + 1)}>下一页</Button></div></div>
    </> : null}
    {drafts.length > 0 && <details className="mt-3 rounded-xl bg-panel-2 p-3 text-[12.5px] text-body"><summary className="cursor-pointer font-medium">查看全部 {drafts.length} 项变更</summary><ul className="mb-0 mt-2 grid list-none gap-2 p-0">{drafts.map(draft => <li key={draft.id} className="flex items-center gap-2"><Pill tone={draft.operation === "delete" ? "bad" : "ok"}>{changeLabels[draft.operation]}</Pill><span className="min-w-0 flex-1 break-all">{draft.line || draft.sourceLine ? `第 ${draft.sourceLine ?? draft.line} 行 · ` : ""}{draft.columns.join(" · ")}</span><Button size="sm" variant="ghost" disabled={mutation.isPending || editing} onClick={() => setDrafts(current => current.filter(value => value.id !== draft.id))}>撤销</Button></li>)}</ul></details>}
    {(dirty || mutation.isSuccess) && <div className="relative z-20 mt-4 flex flex-wrap items-center justify-between gap-3 rounded-xl bg-panel p-3 ring-1 ring-accent md:sticky md:bottom-3">
      <div><strong className="text-[13px] text-accent-ink">{editor ? "正在编辑词条" : `待保存变更 · ${drafts.length} 项`}</strong><p className="m-0 mt-1 text-[12px] text-muted">{editor ? "Enter 确认修改，Esc 取消；确认后可继续编辑其他词条。" : mutation.isSuccess ? "变更已提交，请在 GitHub 审核后合入。" : "保存将创建一个变更 PR，审核合入后生效。"}</p></div>
      <div className="flex gap-2">{editor ? <><Button variant="outline" onClick={cancelEditor}>取消编辑</Button><Button variant="primary" disabled={locked} onClick={stage}>确认词条</Button></> : <><Button variant="outline" disabled={mutation.isPending} onClick={discard}>放弃草稿</Button><Button variant="primary" disabled={!editable || changed || uncertain || mutation.isPending || Boolean(editor) || !drafts.length} onClick={() => mutation.mutate()}>{mutation.isPending ? "正在保存…" : `保存变更${drafts.length ? `（${drafts.length}）` : ""}`}</Button></>}</div>
    </div>}
  </Card>;
}

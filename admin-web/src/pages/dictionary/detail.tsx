import { useEffect, useMemo, useState } from "react";
import { useMutation, useQuery } from "@tanstack/react-query";
import { useBlocker } from "@tanstack/react-router";
import { ExternalLink, Pencil, Plus, RefreshCw, Search } from "lucide-react";
import { APIError, useAPI } from "../../api/client";
import { columnLabels, dictionaryChangeSchema, dictionaryContentSchema } from "../../api/dictionary";
import type { DictionaryEdit, DictionaryRow } from "../../api/dictionary";
import { keys } from "../../api/keys";
import { noPermissionHint, usePermissions } from "../../shell/permissions";
import { Button, buttonVariants } from "../../ui/button";
import { Banner, Card, CardHeader } from "../../ui/card";
import { DetailDrawer } from "../../ui/drawer";
import { Empty, ErrorState, SkeletonRows } from "../../ui/states";
import { Pill } from "../../ui/pill";

const inputClass = "h-10 w-full rounded-[10px] bg-panel-2 px-3 text-[13px] text-ink outline-none focus:ring-[1.5px] focus:ring-accent";
const changeLabels = { add: "新增", replace: "修改", delete: "删除" } as const;
type Draft = DictionaryEdit & { id: string };
type Editor = { operation: "add" | "replace"; line: number; columns: string[] };

export function FileDetail({ path, onDirty }: { path: string; onDirty: (dirty: boolean) => void }) {
  const api = useAPI();
  const { can } = usePermissions();
  const permitted = can("review_dict_pr");
  const custom = path.startsWith("custom/");
  const [page, setPage] = useState(1);
  const [search, setSearch] = useState("");
  const [query, setQuery] = useState("");
  const [drafts, setDrafts] = useState<Draft[]>([]);
  const [sha, setSHA] = useState("");
  const [editor, setEditor] = useState<Editor>();
  const [editorError, setEditorError] = useState("");
  const [uncertain, setUncertain] = useState(false);
  useEffect(() => { const timer = window.setTimeout(() => { setQuery(search.trim()); setPage(1); }, 350); return () => window.clearTimeout(timer); }, [search]);
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
  const openEditor = (row?: DictionaryRow) => {
    pin(); setEditorError("");
    const current = row && drafts.find(draft => draft.line === row.line);
    setEditor(row ? { operation: "replace", line: row.line, columns: current?.columns ?? row.columns } : { operation: "add", line: 0, columns: editLabels.map(() => "") });
  };
  const stage = () => {
    if (!editor) return;
    if (editor.columns.length !== editLabels.length || editor.columns.some(value => !value.trim() || /[\t\r\n]/.test(value))) { setEditorError("请填写每个字段，字段内不能含换行或制表符。"); return; }
    const entry = { ...editor, columns: editor.columns.map(value => value.trim()), id: editor.line ? `line-${editor.line}` : crypto.randomUUID() };
    setDrafts(current => [...current.filter(draft => draft.id !== entry.id), entry]); setEditor(undefined);
  };
  const removeRow = (row: DictionaryRow) => { pin(); setDrafts(current => [...current.filter(draft => draft.line !== row.line), { id: `line-${row.line}`, operation: "delete", line: row.line, columns: [] }]); };
  const discard = () => { setDrafts([]); setSHA(""); setEditor(undefined); setUncertain(false); mutation.reset(); };
  const pullURL = mutation.data?.url ?? (mutation.error instanceof APIError && typeof mutation.error.details.pull_url === "string" ? mutation.error.details.pull_url : undefined);
  return <Card>
    <CardHeader title={path.split("/").pop()} sub={<span className="break-all font-mono">{path}</span>} actions={<>
      <Button size="sm" variant="outline" disabled={mutation.isPending || content.isFetching} onClick={() => void content.refetch()}><RefreshCw size={14} aria-hidden="true" />刷新</Button>
      {data?.url && <a href={data.url} target="_blank" rel="noreferrer" className={buttonVariants({ size: "sm", variant: "outline" })}><ExternalLink size={14} aria-hidden="true" />源文件</a>}
      {data?.editable && <Button size="sm" variant="primary" disabled={!permitted || uncertain || mutation.isPending || changed || drafts.length >= 100} title={permitted ? undefined : noPermissionHint} onClick={() => openEditor()}><Plus size={14} aria-hidden="true" />新增词条</Button>}
    </>} />
    <form onSubmit={event => { event.preventDefault(); setQuery(search.trim()); setPage(1); }} className="mb-3 flex h-10 items-center gap-2 rounded-[10px] bg-panel-2 px-3 text-muted">
      <Search size={16} aria-hidden="true" /><input value={search} maxLength={100} onChange={event => setSearch(event.target.value)} placeholder="搜索当前词库中的词条、编码或译文" aria-label="搜索当前文件词条" className="min-w-0 flex-1 bg-transparent text-[13px] text-ink outline-none placeholder:text-muted" />
    </form>
    {blocker.status === "blocked" && <Banner tone="warn" className="mb-3"><div className="flex flex-wrap items-center justify-between gap-2"><span>离开页面会放弃未提交的词条变更。</span><div className="flex gap-2"><Button size="sm" variant="outline" onClick={() => blocker.reset()}>留在此页</Button><Button size="sm" variant="danger-outline" onClick={() => blocker.proceed()}>放弃并离开</Button></div></div></Banner>}
    {mutation.isSuccess && <Banner tone="ok" className="mb-3">已创建 PR #{mutation.data.number}。<a href={mutation.data.url} target="_blank" rel="noreferrer">查看并审核变更</a>，合入后生效。</Banner>}
    {mutation.isError && <div className="mb-3"><ErrorState error={mutation.error} />{pullURL && <a href={pullURL} target="_blank" rel="noreferrer">查看已创建的 PR</a>}{uncertain && <p className="text-xs text-muted">提交已停止，请先检查仓库的 Pull Request 与 dictionary-admin 分支，再决定是否重新编辑。</p>}</div>}
    {changed && <Banner tone="warn" className="mb-3">文件已更新，当前草稿无法提交。请先记录待提交的内容，再放弃草稿并基于新版本编辑。</Banner>}
    {drafts.length > 0 && <div className="mb-3 rounded-xl bg-accent-soft p-3">
      <div className="mb-2 flex flex-wrap items-center justify-between gap-2"><strong className="text-[13px] text-accent-ink">待提交变更 · {drafts.length} 项</strong><div className="flex gap-2"><Button size="sm" variant="outline" disabled={mutation.isPending} onClick={discard}>放弃草稿</Button><Button size="sm" variant="primary" disabled={!editable || changed || uncertain || mutation.isPending || Boolean(editor)} onClick={() => mutation.mutate()}>{mutation.isPending ? "正在创建…" : "创建 Pull Request"}</Button></div></div>
      <ul className="m-0 grid list-none gap-1 p-0">{drafts.map(draft => <li key={draft.id} className="flex items-center gap-2 text-[12px] text-body"><Pill tone={draft.operation === "delete" ? "bad" : "ok"}>{changeLabels[draft.operation]}</Pill><span className="min-w-0 flex-1 break-all">{draft.line ? `第 ${draft.line} 行 · ` : ""}{draft.columns.join(" · ")}</span><Button size="sm" variant="ghost" disabled={mutation.isPending} onClick={() => setDrafts(current => current.filter(value => value.id !== draft.id))}>撤销</Button></li>)}</ul>
    </div>}
    {content.isPending ? <SkeletonRows rows={8} /> : content.isError ? <ErrorState error={content.error} onRetry={() => void content.refetch()} /> : data ? <>
      <div className="mb-3 flex flex-wrap justify-between gap-2 text-[12px] text-muted"><span>{data.total.toLocaleString("zh-CN")} 条匹配 · {data.encoding}</span><span>{data.editable ? custom ? "只追加 · 翻译可用新行覆盖" : "词包支持增删改" : "上游来源 · 只读"} · <span className="font-mono">{data.sha.slice(0, 8)}</span></span></div>
      <div className="max-h-[620px] overflow-auto rounded-xl ring-1 ring-hair"><table className="w-full min-w-[520px] border-collapse text-left text-[12.5px]">
        <thead className="sticky top-0 bg-panel-2 text-muted"><tr><th className="w-14 px-3 py-2 font-medium">行</th>{labels.map(label => <th key={label} className="px-3 py-2 font-medium">{label}</th>)}{editable && !custom && <th className="px-3 py-2 font-medium">操作</th>}</tr></thead>
        <tbody>{data.rows.map(row => {
          const staged = drafts.find(draft => draft.line === row.line);
          return <tr key={row.line} className="border-t border-hair align-top hover:bg-panel-2"><td className="px-3 py-2 font-mono text-muted">{row.line}</td>{labels.map((label, index) => <td key={label} className="max-w-[300px] break-words px-3 py-2 whitespace-pre-wrap text-body">{staged?.operation === "delete" ? <del>{row.columns[index]}</del> : (staged?.columns ?? row.columns)[index] ?? ""}</td>)}{editable && !custom && <td className="px-3 py-2"><div className="flex gap-1"><Button size="sm" variant="outline" disabled={uncertain || changed || mutation.isPending || drafts.length >= 100} onClick={() => openEditor(row)}><Pencil size={12} aria-hidden="true" />编辑</Button><Button size="sm" variant="danger-outline" disabled={uncertain || changed || mutation.isPending || drafts.length >= 100} onClick={() => removeRow(row)}>删除</Button></div></td>}</tr>;
        })}</tbody>
      </table>{data.rows.length === 0 && <Empty title="没有匹配的词条" />}</div>
      <div className="mt-3 flex flex-wrap items-center justify-between gap-2 text-[12px] text-muted"><span>第 {page} / {Math.max(1, Math.ceil(data.total / 100))} 页 · 每页 100 条</span><div className="flex gap-2"><Button size="sm" variant="outline" disabled={page <= 1 || mutation.isPending} onClick={() => setPage(value => value - 1)}>上一页</Button><Button size="sm" variant="outline" disabled={!data.has_more || mutation.isPending} onClick={() => setPage(value => value + 1)}>下一页</Button></div></div>
    </> : null}
    <DetailDrawer open={Boolean(editor)} onClose={() => setEditor(undefined)} title={editor?.operation === "add" ? "新增词条" : "编辑词条"} sub={path} actions={[{ label: "加入待提交变更", variant: "primary", keepOpen: true, onClick: stage }]}>
      {editor && <div className="grid gap-4">{editLabels.map((label, index) => <label key={label} className="grid gap-1.5 text-[13px] text-body">{label}<input value={editor.columns[index] ?? ""} onChange={event => { const next = [...editor.columns]; next[index] = event.target.value; setEditor({ ...editor, columns: next }); }} aria-label={label} maxLength={300} className={inputClass} /></label>)}<p className="m-0 text-[12px] leading-relaxed text-muted">全拼音节用 ' 分隔，如 shui'shan。中文权重为 1–10000；自定义词库还需符合文件现有范围。最终由仓库 CI 校验读音与重复项。</p>{editorError && <p role="alert" className="m-0 text-[13px] text-bad">{editorError}</p>}</div>}
    </DetailDrawer>
  </Card>;
}

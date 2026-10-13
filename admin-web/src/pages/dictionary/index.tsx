import { useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { BookOpen, ExternalLink, FileText, RefreshCw, Search } from "lucide-react";
import { useAPI } from "../../api/client";
import { dictionarySchema, fileSection, sectionLabels } from "../../api/dictionary";
import type { DictionaryFile, Section } from "../../api/dictionary";
import { keys } from "../../api/keys";
import { PageIntro } from "../../shell/page-intro";
import { Button, buttonVariants } from "../../ui/button";
import { Banner, Card, CardHeader } from "../../ui/card";
import { cn } from "../../ui/cn";
import { FilterChips } from "../../ui/filter-chips";
import { StatGrid, StatTile } from "../../ui/stat-tile";
import { Empty, ErrorState, SkeletonRows } from "../../ui/states";
import { FileDetail } from "./detail";

const sectionOrder: Section[] = ["custom", "packs", "sources"];
export function formatBytes(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / 1024 / 1024).toFixed(1)} MB`;
}
export function fileName(path: string): string { return path.slice(path.lastIndexOf("/") + 1); }

function FileList({ files, selected, onSelect }: { files: DictionaryFile[]; selected?: string; onSelect: (path: string) => void }) {
  if (files.length === 0) return <Empty title="没有匹配的文件" className="py-10" />;
  return <ul className="m-0 grid max-h-[620px] list-none gap-1 overflow-y-auto p-0 pr-1">
    {files.map(file => <li key={file.path}><button type="button" aria-pressed={selected === file.path} onClick={() => onSelect(file.path)}
      className={cn("flex w-full items-center gap-2.5 rounded-xl px-3 py-2.5 text-left transition hover:bg-panel-2", selected === file.path && "bg-accent-soft hover:bg-accent-soft")}>
      <span className={cn("grid h-8 w-8 shrink-0 place-items-center rounded-[9px]", selected === file.path ? "bg-accent text-btn-fg" : "bg-panel-2 text-muted")}><FileText size={16} aria-hidden="true" /></span>
      <span className="min-w-0 flex-1"><span className={cn("block truncate text-[13px]", selected === file.path ? "font-semibold text-accent-ink" : "text-ink")}>{fileName(file.path)}</span><span className="block truncate text-[11.5px] text-muted">{file.path.slice(0, -(fileName(file.path).length + 1))}</span></span>
      <span className="shrink-0 text-[11px] text-muted tabular-nums">{formatBytes(file.size)}</span>
    </button></li>)}
  </ul>;
}

export default function DictionaryPage() {
  const api = useAPI();
  const [section, setSection] = useState<Section>("custom");
  const [fileSearch, setFileSearch] = useState("");
  const [selected, setSelected] = useState<string>();
  const [dirty, setDirty] = useState(false);
  const [pendingPath, setPendingPath] = useState<{ section: Section; path?: string }>();
  const tree = useQuery({ queryKey: keys.page("dictionary", "tree"), queryFn: ({ signal }) => api.get("dictionary", dictionarySchema, { signal }) });
  const files = tree.data?.files;
  const sectionFiles = useMemo(() => (files ?? []).filter(file => fileSection(file) === section && (!fileSearch.trim() || file.path.toLowerCase().includes(fileSearch.trim().toLowerCase()))), [files, fileSearch, section]);
  const active = selected ?? files?.find(file => fileSection(file) === section)?.path;
  const navigate = (nextSection: Section, nextPath?: string) => {
    if (dirty) { setPendingPath({ section: nextSection, path: nextPath }); return; }
    setSection(nextSection); setSelected(nextPath);
  };
  return <div>
    <PageIntro page="dictionary">
      <Link to="/dictpr" className={buttonVariants({ size: "sm", variant: "outline" })}>投稿审核</Link>
      {tree.data?.url && <a href={tree.data.url} target="_blank" rel="noreferrer" className={buttonVariants({ size: "sm", variant: "outline" })}><ExternalLink size={14} aria-hidden="true" />词库仓库</a>}
    </PageIntro>
    {tree.isError ? <ErrorState error={tree.error} onRetry={() => void tree.refetch()} /> : <>
      <StatGrid className="mb-3.5">
        <StatTile label="词库文件" value={tree.data ? tree.data.files.length.toLocaleString("zh-CN") : "—"} sub={tree.data ? `${tree.data.branch} 分支 · ${formatBytes(tree.data.bytes)}` : "正在读取…"} />
        <StatTile label="自定义词库" value={tree.data?.counts.custom ?? "—"} sub="日常补词与翻译修正" />
        <StatTile label="专题词包" value={tree.data?.counts.packs ?? "—"} sub="用户按需导入的专业词库" />
        <StatTile label="原始来源" value={tree.data?.counts.sources ?? "—"} sub="按输入方案管理上游数据" />
      </StatGrid>
      {tree.data?.truncated && <Banner tone="warn" className="mb-3.5">GitHub 返回的目录超过单次读取上限，当前目录不完整。</Banner>}
      {pendingPath && <Banner tone="warn" className="mb-3.5"><div className="flex flex-wrap items-center justify-between gap-2"><span>当前文件有待提交变更，切换文件会放弃这些变更。</span><div className="flex gap-2"><Button size="sm" variant="outline" onClick={() => setPendingPath(undefined)}>继续编辑</Button><Button size="sm" variant="danger-outline" onClick={() => { setDirty(false); setSection(pendingPath.section); setSelected(pendingPath.path); setPendingPath(undefined); }}>放弃并切换</Button></div></div></Banner>}
      <div className="grid min-w-0 items-start gap-3.5 xl:grid-cols-[minmax(280px,0.7fr)_minmax(0,1.7fr)]">
        <Card>
          <CardHeader title={<span className="flex items-center gap-2"><BookOpen size={16} className="text-accent" aria-hidden="true" />词库目录</span>} actions={<Button size="icon" variant="ghost" title="刷新目录" aria-label="刷新词库目录" onClick={() => void tree.refetch()}><RefreshCw size={15} aria-hidden="true" /></Button>} />
          <FilterChips label="词库分类" value={section} onChange={key => navigate(key)} options={sectionOrder.map(key => ({ key, label: sectionLabels[key], count: tree.data?.counts[key] ?? 0 }))} className="mb-3" />
          <label className="mb-3 flex h-9 items-center gap-2 rounded-[10px] bg-panel-2 px-3 text-muted"><Search size={15} aria-hidden="true" /><input value={fileSearch} onChange={event => setFileSearch(event.target.value)} placeholder="搜索文件名或方案" aria-label="筛选词库文件" className="min-w-0 flex-1 bg-transparent text-[12.5px] text-ink outline-none placeholder:text-muted" /></label>
          {tree.isPending ? <SkeletonRows rows={7} /> : <FileList files={sectionFiles} selected={active} onSelect={path => navigate(section, path)} />}
        </Card>
        {active ? <FileDetail key={active} path={active} onDirty={setDirty} /> : <Card><Empty title="没有可显示的文件">请选择分类或调整文件筛选条件。</Empty></Card>}
      </div>
      <p className="mt-3 text-[12.5px] text-muted">自定义词库只追加新词条；专题词包可增删改词条。所有变更提交为 Pull Request，合入后生效。上游来源保持原始数据。</p>
    </>}
  </div>;
}

import { formatNumber } from "../../utils/number";
import { useEffect, useRef, useState } from "react";
import { ChevronLeft, ChevronRight, ExternalLink } from "lucide-react";
import type { Release, ReleaseHistory, ReleasePlatform } from "../../api/release";
import { noPermissionHint } from "../../shell/permissions";
import { Button } from "../../ui/button";
import { Card } from "../../ui/card";
import { cn } from "../../ui/cn";
import { Pill } from "../../ui/pill";
import { Segmented } from "../../ui/segmented";
import { StatGrid, StatTile } from "../../ui/stat-tile";
import { Empty, ErrorState, SkeletonRows } from "../../ui/states";
import { formatSize, noteKindClass, releaseDate, statusLabel, statusTone } from "./shared";

export type DetailHandlers = {
  canTrigger: boolean;
  onTrigger: (platform: ReleasePlatform, version: string) => void;
  onEditNotes: (platform: ReleasePlatform, release: Release) => void;
  onWithdraw: (platform: ReleasePlatform, release: Release) => void;
};

// DetailHeader is the back button, platform name, repository and platform tabs above the history; it renders before the history has loaded.
export function DetailHeader({ platform, platforms, onBack, onPick }: { platform: ReleasePlatform; platforms: readonly ReleasePlatform[]; onBack: () => void; onPick: (id: string) => void }) {
  return <div className="flex flex-wrap items-center gap-3">
    <Button variant="outline" className="h-[34px] pr-3 pl-2 font-normal" onClick={onBack}><ChevronLeft size={16} aria-hidden="true" />全部平台</Button>
    <span className="text-xl font-bold text-ink">{platform.name}</span>
    <span className="min-w-0 font-mono text-[13px] [overflow-wrap:anywhere] text-muted">{platform.repo} · tag 前缀 {platform.tag_prefix}</span>
    {platforms.length > 1 && <div className="max-w-full overflow-x-auto min-[760px]:ml-auto">
      <Segmented label="平台" value={platform.id} onChange={onPick} className="rounded-xl p-1" options={platforms.map(p => ({ value: p.id, label: p.name }))} />
    </div>}
  </div>;
}

export function DetailBody({ platform, history, loading, error, onRetry, focusTag, handlers }: {
  platform: ReleasePlatform;
  history: ReleaseHistory | undefined;
  loading: boolean;
  error: unknown;
  onRetry: () => void;
  focusTag: string | undefined;
  handlers: DetailHandlers;
}) {
  const releases = history?.releases ?? [];
  // One row is open at a time; the focused release (from search or a notification) or the newest one opens first.
  const [open, setOpen] = useState<string | null | undefined>(undefined);
  const openTag = open === undefined ? (focusTag ?? releases[0]?.tag ?? null) : open;
  const current = releases.find(r => r.status !== "withdrawn");
  const published = releases.filter(r => r.status === "released").length;
  const downloads = releases.reduce((sum, r) => sum + r.downloads, 0);
  const triggerTitle = !handlers.canTrigger ? noPermissionHint : !platform.workflow ? "未配置发布流水线（release_workflow）" : undefined;
  return <>
    <StatGrid>
      <StatTile label="当前版本" value={<span className="font-mono">{current?.version ?? "—"}</span>} />
      <StatTile label="历史版本" value={history ? releases.length : "—"} />
      <StatTile label="正式发布" value={history ? published : "—"} />
      <StatTile label="累计下载" value={history ? formatNumber(downloads, "en-US") : "—"} sub="GitHub Release 资产下载次数" />
    </StatGrid>
    <Card className="overflow-hidden p-0">
      <div className="flex flex-wrap items-center justify-between gap-3 border-b border-hair px-5 py-3">
        <h2 className="m-0 text-[15px] font-bold text-ink">发布历史</h2>
        <Button size="sm" variant="primary" disabled={!handlers.canTrigger || !platform.workflow} title={triggerTitle}
          onClick={() => handlers.onTrigger(platform, current && current.status !== "released" ? current.version : "")}>触发发布</Button>
      </div>
      {loading ? <SkeletonRows rows={4} /> : error ? <ErrorState className="m-4" error={error} onRetry={onRetry} /> : releases.length === 0
        ? <Empty title="还没有发布">{`仓库 ${platform.repo} 中没有以 ${platform.tag_prefix} 开头的 Release。`}</Empty>
        : <ul className="m-0 list-none p-0">
          {releases.map(release => <HistoryRow key={release.tag} release={release} focused={focusTag === release.tag} open={openTag === release.tag} onToggle={() => setOpen(openTag === release.tag ? null : release.tag)}
            canTrigger={handlers.canTrigger} onEditNotes={() => handlers.onEditNotes(platform, release)} onWithdraw={() => handlers.onWithdraw(platform, release)} />)}
        </ul>}
    </Card>
  </>;
}

function HistoryRow({ release, focused, open, onToggle, canTrigger, onEditNotes, onWithdraw }: { release: Release; focused: boolean; open: boolean; onToggle: () => void; canTrigger: boolean; onEditNotes: () => void; onWithdraw: () => void }) {
  const bodyID = `release-${release.id}`;
  const rowRef = useRef<HTMLLIElement>(null);
  // A release opened from the global search may sit far down the history; bring it into view once.
  useEffect(() => {
    if (focused) rowRef.current?.scrollIntoView({ block: "center" });
  }, [focused]);
  return <li ref={rowRef} className="border-b border-hair last:border-b-0">
    <button type="button" onClick={onToggle} aria-expanded={open} aria-controls={bodyID}
      className="grid w-full grid-cols-[minmax(0,1fr)] items-center gap-x-3 gap-y-1 px-5 py-3.5 text-left transition hover:bg-panel-2 min-[820px]:grid-cols-[minmax(220px,1.6fr)_110px_100px_110px]">
      <span className="flex min-w-0 items-center gap-2.5">
        <ChevronRight size={14} strokeWidth={2.2} aria-hidden="true" className={cn("shrink-0 text-muted transition-transform duration-200", open && "rotate-90")} />
        <span className="truncate font-mono text-[15px] font-medium text-ink">{release.version}</span>
        <Pill tone={statusTone[release.status]} className="h-5 px-2 text-[11.5px]">{statusLabel[release.status]}</Pill>
      </span>
      <span className="text-[13px] text-muted max-[819px]:pl-6">{releaseDate(release)}</span>
      <span className="truncate text-[13px] text-body max-[819px]:pl-6">{release.author ? `@${release.author}` : "—"}</span>
      <span className="text-[13px] text-muted tabular-nums max-[819px]:pl-6">{release.assets.length ? `下载 ${formatNumber(release.downloads, "en-US")}` : "—"}</span>
    </button>
    {open && <div id={bodyID} className="px-5 pt-0.5 pb-[18px] min-[760px]:pl-11">
      {release.notes.length === 0 ? <p className="m-0 py-1 text-[13px] text-muted">发布说明为空。</p> : release.notes.map((note, index) => (
        // biome-ignore lint/suspicious/noArrayIndexKey: note lines have no identity and repeat text freely
        <div key={index} className="flex gap-2.5 py-1 text-[13.5px] leading-[1.7]">
          {note.kind && <span className={cn("mt-0.5 inline-flex h-5 shrink-0 items-center rounded-md px-2 text-[11.5px] font-semibold", noteKindClass[note.kind] ?? noteKindClass.改进)}>{note.kind}</span>}
          <span className="min-w-0 [overflow-wrap:anywhere] text-body">{note.text}</span>
        </div>
      ))}
      {release.assets.length > 0 && <div className="mt-3 flex flex-wrap gap-2">
        {release.assets.map(asset => <a key={asset.name} href={asset.url || undefined} target="_blank" rel="noopener noreferrer" title={`下载 ${formatNumber(asset.downloads, "en-US")} 次`}
          className="inline-flex h-[30px] max-w-full items-center gap-2 rounded-lg bg-panel-2 px-2.5 text-[12.5px] text-ink no-underline hover:ring-1 hover:ring-hair-2">
          <span className="truncate font-mono">{asset.name}</span>
          <span className="shrink-0 text-muted">{formatSize(asset.size)}</span>
        </a>)}
      </div>}
      <div className="mt-3.5 flex flex-wrap gap-2">
        <Button size="sm" variant="outline" className="h-8 rounded-[9px] px-3 text-[13px] font-normal" disabled={!canTrigger} title={canTrigger ? undefined : noPermissionHint} onClick={onEditNotes}>编辑说明</Button>
        {release.status === "released" && <Button size="sm" variant="danger-outline" className="h-8 rounded-[9px] px-3 text-[13px] font-normal" disabled={!canTrigger} title={canTrigger ? undefined : noPermissionHint} onClick={onWithdraw}>撤回此版本</Button>}
        {release.url && <a href={release.url} target="_blank" rel="noopener noreferrer" className="inline-flex h-8 items-center gap-1.5 rounded-[9px] px-3 text-[13px] text-body no-underline hover:bg-panel-2 hover:text-ink">
          <ExternalLink size={14} aria-hidden="true" />在 GitHub 打开
        </a>}
      </div>
    </div>}
  </li>;
}

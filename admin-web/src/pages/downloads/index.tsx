import { formatNumber } from "../../utils/number";
import { useMemo } from "react";
import { useQuery } from "@tanstack/react-query";
import { useAPI } from "../../api/client";
import { channelLabel, downloadsSummarySchema, platformKey, platformLabel, platformOptions } from "../../api/downloads";
import type { DownloadRow, DownloadsSummary } from "../../api/downloads";
import { keys } from "../../api/keys";
import { PageIntro } from "../../shell/page-intro";
import { MirrorSettings } from "./mirror";
import { usePageSearch, useSetPageSearch } from "../../shell/page-search";
import { Banner } from "../../ui/card";
import { CellText, DataTable } from "../../ui/data-table";
import type { Column } from "../../ui/data-table";
import { FilterChips } from "../../ui/filter-chips";
import type { ChipOption } from "../../ui/filter-chips";
import { StatGrid, StatTile } from "../../ui/stat-tile";
import { Skeleton } from "../../ui/states";

const knownPlatforms = new Set<string>(platformOptions.map(option => option.key));

function rowId(row: DownloadRow): string {
  return [row.source, row.platform, row.version, row.artifact ?? "", row.channel ?? "", row.repo ?? "", row.tag ?? ""].join("\u0000");
}

function rowSearchText(row: DownloadRow): string {
  return `${platformLabel(row.platform)} ${row.platform} ${row.version} ${row.artifact ?? ""} ${channelLabel(row)} ${row.repo ?? ""} ${row.tag ?? ""}`;
}

// WeekBar is the design's 7-day bar: a 6px accent fill relative to the busiest row, followed by the count.
function WeekBar({ value, max }: { value: number; max: number }) {
  return <div className="flex min-w-0 items-center gap-2.5">
    <div className="h-1.5 min-w-0 flex-1 overflow-hidden rounded-[3px] bg-panel-2" aria-hidden="true">
      <div className="h-full rounded-[3px] bg-accent" style={{ width: `${max > 0 ? (value / max) * 100 : 0}%` }} />
    </div>
    <span className="min-w-10 text-right text-xs text-muted tabular-nums">{formatNumber(value, "en-US")}</span>
  </div>;
}

function columnsFor(max: number): Column<DownloadRow>[] {
  return [
    { id: "platform", header: "平台 / 版本", width: "minmax(140px,1fr)", cell: row => <CellText title={platformLabel(row.platform)} sub={row.version} /> },
    {
      id: "artifact", header: "安装包", width: "minmax(150px,1.2fr)",
      cell: row => row.artifact
        ? <span className="block truncate" title={row.source === "github_release" && row.repo ? `${row.repo} · ${row.tag ?? ""}` : undefined}>{row.artifact}</span>
        : <span className="text-muted">未上报</span>,
    },
    { id: "channel", header: "渠道", width: "minmax(140px,1fr)", cell: row => <span className="block truncate text-muted">{channelLabel(row)}</span> },
    { id: "today", header: "今日", width: "70px", align: "right", cell: row => <span className="tabular-nums">{formatNumber(row.today, "en-US")}</span> },
    { id: "week", header: "近 7 天", width: "minmax(160px,1.3fr)", cell: row => <WeekBar value={row.week} max={max} /> },
  ];
}

function mirrorShare(summary: DownloadsSummary): { value: string; sub: string } {
  if (!summary.channel_reported) return { value: "—", sub: "客户端未上报渠道" };
  if (summary.mirror_share === null) return { value: "—", sub: "近 7 天暂无下载" };
  return { value: `${Math.round(summary.mirror_share * 100)}%`, sub: `近 7 天 ${formatNumber(summary.totals.mirror_week, "en-US")} 次` };
}

function Stats({ summary }: { summary: DownloadsSummary | undefined }) {
  if (!summary) {
    return <StatGrid>{["今日下载", "近 7 天", "国内镜像占比", "GitHub Release 近 7 天"].map(label => <StatTile key={label} label={label} value={<Skeleton className="mt-1 h-6 w-20" />} />)}</StatGrid>;
  }
  const share = mirrorShare(summary);
  return <StatGrid>
    <StatTile label="今日下载" value={formatNumber(summary.totals.today, "en-US")} sub={`UTC ${summary.day}`} />
    <StatTile label="近 7 天" value={formatNumber(summary.totals.week, "en-US")} sub="含 GitHub Release 快照" />
    <StatTile label="国内镜像占比" value={share.value} sub={share.sub} />
    <StatTile label="GitHub Release 近 7 天" value={formatNumber(summary.totals.github_week, "en-US")} sub={summary.snapshot_day ? `快照截至 ${summary.snapshot_day}` : "暂无下载快照"} />
  </StatGrid>;
}

export default function DownloadsPage() {
  const api = useAPI();
  const { platform: requested = "all" } = usePageSearch();
  // An unknown ?platform= value (a stale or hand-edited link) shows every row, matching the highlighted 全部 chip.
  const platform = requested === "all" || requested === "other" || knownPlatforms.has(requested) ? requested : "all";
  const setSearch = useSetPageSearch();
  const query = useQuery({
    queryKey: keys.page("downloads", "summary"),
    queryFn: ({ signal }) => api.get("downloads/summary", downloadsSummarySchema, { signal }),
  });
  const rows = query.data?.rows;

  const chips = useMemo(() => {
    const counts = new Map<string, number>();
    for (const row of rows ?? []) {
      const key = knownPlatforms.has(platformKey(row.platform)) ? platformKey(row.platform) : "other";
      counts.set(key, (counts.get(key) ?? 0) + 1);
    }
    const options: ChipOption<string>[] = [{ key: "all", label: "全部", count: rows?.length ?? 0 }, ...platformOptions.map(option => ({ key: option.key, label: option.label, count: counts.get(option.key) ?? 0 }))];
    if (counts.has("other") || platform === "other") options.push({ key: "other", label: "其他", count: counts.get("other") ?? 0 });
    return options;
  }, [rows, platform]);

  const visible = useMemo(() => rows?.filter(row => {
    if (platform === "all") return true;
    const key = platformKey(row.platform);
    return platform === "other" ? !knownPlatforms.has(key) : key === platform;
  }), [rows, platform]);
  const max = useMemo(() => Math.max(0, ...(rows ?? []).map(row => row.week)), [rows]);
  const columns = useMemo(() => columnsFor(max), [max]);
  const empty = rows && rows.length === 0;

  return <>
    <PageIntro page="downloads" />
    <div className="grid gap-4">
      <Stats summary={query.data} />
      {query.data?.truncated && <Banner tone="warn">分组过多，表格只显示近 7 天下载量最高的部分分组；统计卡片仍按全部下载计算。</Banner>}
      <DataTable
        ariaLabel="下载记录"
        data={visible}
        loading={query.isPending}
        error={query.error}
        onRetry={() => void query.refetch()}
        columns={columns}
        getRowId={rowId}
        toolbar={<FilterChips label="平台" value={platform} onChange={key => setSearch({ platform: key === "all" ? undefined : key })} options={chips} />}
        searchText={rowSearchText}
        emptyText={empty ? "近 7 天没有下载上报，也没有 GitHub Release 下载快照" : "这一栏是空的"}
        minWidth="720px"
      />
      <p className="m-0 text-xs leading-relaxed text-muted">按 UTC 自然日统计。客户端与官网镜像的下载来自遥测上报，安装包和渠道需要上报方带上 artifact / channel；GitHub Release 渠道取每日下载快照的差值，不依赖客户端。</p>
      <MirrorSettings />
    </div>
  </>;
}

import { formatNumber, formatRate } from "../../utils/number";
import { deviceRateSub, excludedVersionsNote, INSUFFICIENT, sessionRateText } from "../../utils/crash-rate";
import { useCallback, useMemo } from "react";
import { useQueries, useQuery } from "@tanstack/react-query";
import { errorMessage, isGithubDisabled, useAPI } from "../../api/client";
import { keys } from "../../api/keys";
import { comparePlatforms, crashGroupsSchema, crashIssueTargetSchema, crashStatusLabel, crashStatusTone, crashTrend, platformName } from "../../api/crash";
import type { CrashGroup, CrashIssueTarget, CrashStatus, SessionVersion } from "../../api/crash";
import { PageIntro } from "../../shell/page-intro";
import { usePageSearch, useSetPageSearch } from "../../shell/page-search";
import { noPermissionHint, usePermissions } from "../../shell/permissions";
import { Button } from "../../ui/button";
import { Banner } from "../../ui/card";
import { cn } from "../../ui/cn";
import { CountBar } from "../../ui/count-bar";
import { CellText, DataTable } from "../../ui/data-table";
import type { Column } from "../../ui/data-table";
import { FilterChips } from "../../ui/filter-chips";
import { Pill } from "../../ui/pill";
import { StatGrid, StatTile } from "../../ui/stat-tile";
import { useToast } from "../../ui/toast";
import { useCrashActions } from "./actions";
import { CrashDrawer } from "./drawer";

const ALL = "all";
const SIGNATURE = /^[0-9a-f]{16}$/;
const UNREPORTED = "客户端未上报";

// SessionVersions 列出近 7 天有会话的各版本；会话数据不足的版本不给比率，也不计入顶部的无崩溃会话率。默认折叠，避免把崩溃分组表挤到下面。
function SessionVersions({ versions }: { versions: SessionVersion[] }) {
  const insufficient = versions.filter(v => v.insufficient).length;
  return <details className="mb-3.5 rounded-2xl bg-panel px-[18px] py-3.5 text-[13px] ring-1 ring-hair">
    <summary className="cursor-pointer text-muted">各版本会话统计：{formatNumber(versions.length)} 个版本{insufficient ? `，${excludedVersionsNote(insufficient)}` : ""}</summary>
    <p className="m-0 mt-2 text-[12.5px] text-muted">近 7 天。每台活跃设备平均正常会话不足 0.5 次的版本视为会话数据不足（客户端漏报正常结束的会话），不给比率也不计入合计。</p>
    <div className="mt-2 overflow-x-auto">
      <table className="w-full border-collapse text-left" aria-label="各版本无崩溃会话率">
        <thead><tr className="border-b border-hair text-muted"><th className="py-2 pr-4 font-medium">平台</th><th className="py-2 pr-4 font-medium">版本</th><th className="py-2 pr-4 font-medium">正常会话</th><th className="py-2 pr-4 font-medium">崩溃会话</th><th className="py-2 pr-4 font-medium">活跃设备</th><th className="py-2 font-medium">无崩溃会话率</th></tr></thead>
        <tbody>{versions.map(v => <tr key={`${v.platform}/${v.version}`} className="border-b border-hair last:border-b-0">
          <td className="py-2 pr-4 text-ink">{platformName(v.platform)}</td>
          <td className="py-2 pr-4 font-mono text-ink">{v.version}</td>
          <td className="py-2 pr-4 tabular-nums text-ink">{formatNumber(v.sessions)}</td>
          <td className="py-2 pr-4 tabular-nums text-ink">{formatNumber(v.crashed_sessions)}</td>
          <td className="py-2 pr-4 tabular-nums text-ink">{formatNumber(v.active_devices)}</td>
          <td className="py-2 tabular-nums text-ink">{v.insufficient ? <Pill tone="warn">{INSUFFICIENT}</Pill> : formatRate(v.crash_free_rate)}</td>
        </tr>)}</tbody>
      </table>
    </div>
  </details>;
}

export default function CrashPage() {
  const api = useAPI();
  const toast = useToast();
  const { can } = usePermissions();
  const { platform = ALL, focus } = usePageSearch();
  const setSearch = useSetPageSearch();
  const { overrides, issueURLs, setStatus, createIssue } = useCrashActions();
  const filter = platform === ALL ? "" : platform;

  const list = useQuery({
    queryKey: keys.page("crash", "groups", filter),
    queryFn: ({ signal }) => api.get(`crash-groups${filter ? `?platform=${encodeURIComponent(filter)}` : ""}`, crashGroupsSchema, { signal }),
  });
  const rows = useMemo(() => list.data?.items.map(group => {
    const status = overrides[group.signature];
    const issueURL = group.issue_url ? undefined : issueURLs[group.signature];
    return status || issueURL ? { ...group, status: status ?? group.status, issue_url: issueURL ?? group.issue_url } : group;
  }), [list.data, overrides, issueURLs]);

  // Where an issue goes depends only on the group's platform, so one group per platform is asked (GET .../issue), preferring the groups a row could create an issue for; any answer also tells whether admin.github is configured at all.
  const probes = useMemo(() => {
    const byPlatform = new Map<string, string>();
    const items = list.data?.items ?? [];
    for (const group of items) if (group.status === "open" && !group.issue_url && !byPlatform.has(group.platform)) byPlatform.set(group.platform, group.signature);
    if (byPlatform.size === 0 && items[0]) byPlatform.set(items[0].platform, items[0].signature);
    return [...byPlatform];
  }, [list.data]);
  // configured[platform] is true when the platform has an issue repository, false when it has none, and absent while unknown (loading or failed); only true enables 建 Issue, so a row never shows a success toast for a request that is bound to fail. combine is memoized so the combined result stays the same object while the query results do not change.
  const combine = useCallback((results: readonly { data?: CrashIssueTarget; error: unknown }[]) => {
    const configured: Record<string, boolean> = {};
    results.forEach((result, i) => {
      const platform = probes[i]?.[0];
      if (platform !== undefined && result.data) configured[platform] = result.data.target !== null;
    });
    return { githubDisabled: results.some(result => isGithubDisabled(result.error)), configured };
  }, [probes]);
  const { githubDisabled, configured } = useQueries({
    queries: probes.map(([, signature]) => ({
      queryKey: keys.page("crash", "issue-target", signature),
      queryFn: ({ signal }: { signal: AbortSignal }) => api.get(`crash-groups/${signature}/issue`, crashIssueTargetSchema, { signal }),
      retry: false,
      staleTime: 60_000,
    })),
    combine,
  });
  const canTriage = can("triage_issues");

  // 搜索、通知和手动打开共用 URL 中的分组签名，历史导航不会被本地选中项遮住。
  const openSignature = focus && SIGNATURE.test(focus) ? focus : null;
  const closeDrawer = () => {
    if (focus) setSearch({ focus: undefined });
  };

  const platformOptions = useMemo(() => {
    const counts = [...(list.data?.platforms ?? [])].sort((a, b) => comparePlatforms(a.platform, b.platform));
    const options = [{ key: ALL, label: "全部", count: counts.reduce((sum, p) => sum + p.count, 0) }, ...counts.map(p => ({ key: p.platform, label: platformName(p.platform), count: p.count }))];
    // A platform taken from the URL that has no groups still shows as the active chip.
    if (filter && list.data && !counts.some(p => p.platform === filter)) options.push({ key: filter, label: platformName(filter), count: 0 });
    return options;
  }, [list.data, filter]);

  const max = useMemo(() => Math.max(1, ...(rows ?? []).map(group => group.count_7d)), [rows]);
  const columns = useMemo<Column<CrashGroup>[]>(() => [
    {
      id: "signature", header: "崩溃签名", width: "minmax(280px,2.4fr)",
      cell: group => <CellText mono strong={false} title={<span title={group.title}>{group.title || group.signature}</span>}
        sub={group.devices_7d === null ? "影响设备未上报" : `影响 ${formatNumber(group.devices_7d)} 台设备`} />,
    },
    { id: "platform", header: "平台 / 版本", width: "120px", cell: group => <CellText strong={false} title={platformName(group.platform)} sub={<span className="font-mono">{group.version}</span>} /> },
    {
      id: "count", header: "7 天次数", width: "minmax(120px,1fr)",
      cell: group => <CountBar value={group.count_7d} max={max} tone={group.status === "fixed" ? "accent" : "bad"} className="gap-2" />,
    },
    {
      id: "trend", header: "趋势", width: "70px",
      cell: group => {
        const trend = crashTrend(group);
        return <span className={cn("font-semibold tabular-nums", trend.rising ? "text-bad" : "text-muted")}>{trend.text}</span>;
      },
    },
    { id: "status", header: "状态", width: "90px", cell: group => <Pill tone={crashStatusTone[group.status]}>{crashStatusLabel[group.status]}</Pill> },
    {
      id: "actions", header: "", width: "150px", align: "right",
      // An open group that already has an issue (reopened after 已知问题) gets 查看堆栈, since a second issue would be refused.
      cell: group => group.status === "open" && !group.issue_url
        ? <Button size="sm" variant="primary" disabled={!canTriage || githubDisabled || configured[group.platform] !== true}
          title={!canTriage ? noPermissionHint : githubDisabled ? "未配置 GitHub 集成" : configured[group.platform] === false ? "该平台未配置 Issue 仓库" : configured[group.platform] === undefined ? "正在确认该平台的 Issue 仓库…" : undefined}
          onClick={event => { event.stopPropagation(); createIssue(group); }}>建 Issue</Button>
        : <Button size="sm" variant="outline" onClick={event => { event.stopPropagation(); setSearch({ focus: group.signature }); }}>查看堆栈</Button>,
    },
  ], [max, canTriage, githubDisabled, configured, createIssue, setSearch]);

  const onStatus = (group: CrashGroup, to: CrashStatus, text: string) => {
    setStatus(group, to, text).catch((error: unknown) => toast(`操作失败：${errorMessage(error)}`));
  };

  const summary = list.data?.summary;
  const insufficientVersions = summary ? summary.versions.filter(v => v.insufficient).length : 0;
  const sessionSub = !summary ? undefined
    : summary.versions.length === 0 ? (summary.crash_free_rate === null ? UNREPORTED : "近 7 天")
    : insufficientVersions ? `近 7 天，${excludedVersionsNote(insufficientVersions)}` : "近 7 天";
  return <>
    <PageIntro page="crash" />
    <StatGrid className="mb-3.5">
      <StatTile label="今日安装上报" value={summary?.installs_today == null ? "—" : formatNumber(summary.installs_today)} sub={summary?.installs_today === null ? UNREPORTED : "今天上报过的安装"} />
      <StatTile label="无崩溃会话" value={summary ? sessionRateText(summary.crash_free_rate, summary.versions.length, insufficientVersions) : "—"} sub={sessionSub} />
      <StatTile label="崩溃设备占比" value={formatRate(summary?.device_crash_rate)}
        sub={!summary ? undefined : summary.active_devices_7d === null ? UNREPORTED : `近 7 天 ${deviceRateSub(summary.devices_7d ?? 0, summary.active_devices_7d)}`} />
      <StatTile label="崩溃分组" value={summary ? formatNumber(summary.groups) : "—"} sub={summary ? `近 7 天崩溃 ${formatNumber(summary.crashes_7d)} 次` : undefined} />
      <StatTile label="影响设备" value={summary?.devices_7d == null ? "—" : formatNumber(summary.devices_7d)} sub={summary?.devices_7d === null ? UNREPORTED : "近 7 天"} />
    </StatGrid>
    {summary && summary.versions.length > 0 && <SessionVersions versions={summary.versions} />}
    {githubDisabled && <Banner tone="warn" className="mb-3.5">
      <b className="text-ink">未配置 GitHub 集成。</b>服务端没有配置 admin.github，暂时不能从崩溃分组建 Issue；可以在详情里手动标记为已知问题或已修复。
    </Banner>}
    {list.data?.has_more && <p className="m-0 mb-2 text-[12.5px] text-muted">只显示近 7 天次数最多、最近出现的 500 个分组。</p>}
    <DataTable
      ariaLabel="崩溃分组"
      data={rows}
      loading={list.isPending}
      error={list.error}
      onRetry={() => void list.refetch()}
      columns={columns}
      getRowId={group => group.signature}
      toolbar={<FilterChips label="平台" value={platform} onChange={key => setSearch({ platform: key === ALL ? undefined : key })} options={platformOptions} />}
      searchText={group => `${group.title} ${group.signature} ${platformName(group.platform)} ${group.version} ${crashStatusLabel[group.status]}`}
      onRowClick={group => setSearch({ focus: group.signature })}
      emptyText={filter ? "这个平台还没有崩溃分组" : "还没有崩溃分组，客户端上报崩溃后会按签名自动分组。"}
      minWidth="880px"
    />
    <CrashDrawer signature={openSignature} override={openSignature ? overrides[openSignature] : undefined} issueURL={openSignature ? issueURLs[openSignature] : undefined} canTriage={canTriage}
      onClose={closeDrawer} onStatus={onStatus} onCreateIssue={createIssue} />
  </>;
}

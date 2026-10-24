import { formatNumber, formatRate } from "../../utils/number";
import { platformName } from "../../utils/platform";
import { deviceRateSub, excludedVersionsNote, INSUFFICIENT, sessionRateText } from "../../utils/crash-rate";
import type { ReactNode } from "react";
import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { useAPI } from "../../api/client";
import { comparePlatforms } from "../../api/crash";
import { keys } from "../../api/keys";
import { overviewSchema } from "../../api/overview";
import type { Overview, OverviewService, ServiceState } from "../../api/overview";
import { communityNavItems } from "../../nav";
import { useCommunityCounts } from "../../hooks/use-community-counts";
import { PageIntro } from "../../shell/page-intro";
import { useShell } from "../../shell/shell-data";
import { Card, CardHeader } from "../../ui/card";
import { AreaChart, ChartEmpty, ChartLegend } from "../../ui/charts";
import { Dot } from "../../ui/pill";
import type { Tone } from "../../ui/pill";
import { ProgressBar } from "../../ui/progress";
import { StatGrid, StatTile } from "../../ui/stat-tile";
import { Empty, ErrorState, Skeleton, SkeletonRows } from "../../ui/states";

const activeSeries = [
  { key: "windows", label: "Windows", color: "var(--chart-1)" },
  { key: "macos", label: "macOS", color: "var(--chart-2)" },
  { key: "linux", label: "Linux", color: "var(--chart-3)" },
  { key: "android", label: "Android", color: "var(--chart-4)" },
  { key: "ios", label: "iOS", color: "var(--chart-5)" },
  { key: "harmonyos", label: "HarmonyOS", color: "var(--chart-6)" },
] as const;

const stateTone: Record<ServiceState, Tone> = { ok: "ok", degraded: "warn", down: "bad", unknown: "mute" };
const stateLabel: Record<ServiceState, string> = { ok: "运行正常", degraded: "性能降级", down: "服务中断", unknown: "暂无探测数据" };

// percentDelta compares two period totals as "↑ 12.4%"; nothing when the previous period is empty, since a ratio against zero means nothing.
function percentDelta(current: number, previous: number): { text: string; tone: "ok" | "bad" } | undefined {
  if (previous <= 0) return undefined;
  const change = ((current - previous) / previous) * 100;
  return { text: `${change >= 0 ? "↑" : "↓"} ${Math.abs(change).toFixed(1)}%`, tone: change >= 0 ? "ok" : "bad" };
}

// pointsDelta compares two rates (0–1) in percentage points, as "↓ 0.08".
function pointsDelta(current: number | null, previous: number | null): { text: string; tone: "ok" | "bad" } | undefined {
  if (current === null || previous === null) return undefined;
  const change = (current - previous) * 100;
  return { text: `${change >= 0 ? "↑" : "↓"} ${Math.abs(change).toFixed(2)}`, tone: change >= 0 ? "ok" : "bad" };
}

function latency(ms: number | null): string {
  if (ms === null) return "—";
  return ms >= 1000 ? `${(ms / 1000).toFixed(1)}s` : `${ms}ms`;
}

function KPIs({ data }: { data: Overview }) {
  const shell = useShell().data;
  const downloads = percentDelta(data.downloads_30d, data.downloads_prev_30d);
  const active = data.telemetry.active ? percentDelta(data.active_devices_7d, data.active_devices_prev_7d) : undefined;
  const crashFree = pointsDelta(data.crash_free_rate, data.crash_free_rate_prev);
  const crashFreeSub = data.crash_free_rate !== null
    ? (data.crash_top ? `${platformName(data.crash_top.platform)} ${data.crash_top.version} 拖累` : "近 7 天会话")
    : !data.telemetry.sessions ? "客户端未上报" : data.crash_platforms.some(p => p.versions > 0) ? INSUFFICIENT : "近 7 天没有会话";
  const pending = shell?.pending;
  return <StatGrid kpi>
    <StatTile size="kpi" label="累计下载" value={formatNumber(data.downloads)} delta={downloads?.text} deltaTone={downloads?.tone}
      sub={downloads ? "近 30 天较前 30 天" : `近 30 天 ${formatNumber(data.downloads_30d)} 次`} />
    <StatTile size="kpi" label="7 日活跃设备" value={data.telemetry.active ? formatNumber(data.active_devices_7d) : "—"} delta={active?.text} deltaTone={active?.tone}
      sub={data.telemetry.active ? "全平台合计" : "客户端未上报"} />
    <StatTile size="kpi" label="无崩溃会话率" value={formatRate(data.crash_free_rate)} delta={crashFree?.text} deltaTone={crashFree?.tone}
      sub={data.device_crash_rate === null ? crashFreeSub : `${crashFreeSub} · 崩溃设备 ${formatRate(data.device_crash_rate)}`} />
    <StatTile size="kpi" label="待审核" value={pending ? formatNumber(pending.dict_prs + pending.community + pending.issues) : "—"}
      sub={pending ? `词库 PR ${pending.dict_prs} · 社区 ${pending.community} · Issue ${pending.issues}` : "正在加载…"} />
  </StatGrid>;
}

function ActiveDevices({ data }: { data: Overview }) {
  const rows = data.active_devices_daily;
  return <Card>
    <CardHeader title={`近 ${data.range_days} 天活跃设备`} actions={data.telemetry.active ? <ChartLegend series={activeSeries} /> : undefined} />
    {data.telemetry.active && rows.length > 0
      ? <AreaChart data={rows} xKey="day" series={activeSeries} filled={false} height={220} formatX={day => day.slice(5)} ariaLabel={`近 ${data.range_days} 天各平台活跃设备数`} />
      : <ChartEmpty>客户端未上报活跃设备数据</ChartEmpty>}
  </Card>;
}

function PlatformActive({ data }: { data: Overview }) {
  const rows = Object.entries(data.platform_active_7d).sort((a, b) => b[1] - a[1]);
  const max = rows.length ? rows[0][1] : 0;
  return <Card>
    <CardHeader title="各平台活跃" sub="近 7 天去重设备" />
    {rows.length === 0
      ? <Empty title={data.telemetry.active ? "近 7 天没有活跃设备" : "客户端未上报"} className="py-8" />
      : <ul className="m-0 grid list-none gap-3.5 p-0 pt-1">
        {rows.map(([platform, devices]) => <li key={platform} className="min-w-0">
          <div className="mb-1.5 flex items-baseline justify-between gap-3 text-[13px]">
            <span className="truncate text-ink">{platformName(platform)}</span>
            <span className="text-muted tabular-nums">{formatNumber(devices, "en-US")}</span>
          </div>
          <ProgressBar value={devices} max={max} label={`${platformName(platform)} 活跃设备`} />
        </li>)}
      </ul>}
  </Card>;
}

// CrashPlatforms 把各平台的无崩溃会话率和崩溃设备占比并排列出；会话率只合计会话数据充分的版本，被排除的版本数写在说明列。
function CrashPlatforms({ data }: { data: Overview }) {
  const rows = [...data.crash_platforms].sort((a, b) => comparePlatforms(a.platform, b.platform));
  return <Card>
    <CardHeader title="各平台稳定性" sub="近 7 天。每台活跃设备平均正常会话不足 0.5 次的版本视为会话数据不足，不计入无崩溃会话率；崩溃设备占比 = 有崩溃的设备 ÷ 活跃设备" />
    {rows.length === 0
      ? <Empty title={data.telemetry.active || data.telemetry.sessions ? "近 7 天没有会话或活跃设备" : "客户端未上报"} className="py-8" />
      : <div className="overflow-x-auto">
        <table className="w-full border-collapse text-left text-[13px]" aria-label="各平台无崩溃会话率与崩溃设备占比">
          <thead><tr className="border-b border-hair text-muted"><th className="py-2 pr-4 font-medium">平台</th><th className="py-2 pr-4 font-medium">无崩溃会话率</th><th className="py-2 pr-4 font-medium">崩溃设备占比</th><th className="py-2 font-medium">说明</th></tr></thead>
          <tbody>{rows.map(row => <tr key={row.platform} className="border-b border-hair last:border-b-0">
            <td className="py-2 pr-4 text-ink">{platformName(row.platform)}</td>
            <td className="py-2 pr-4 tabular-nums text-ink">{sessionRateText(row.crash_free_rate, row.versions, row.insufficient_versions)}</td>
            <td className="py-2 pr-4 tabular-nums text-ink">{formatRate(row.device_crash_rate)} <span className="text-muted">{deviceRateSub(row.crash_devices_7d, row.active_devices_7d)}</span></td>
            <td className="py-2 text-muted">{excludedVersionsNote(row.insufficient_versions)}</td>
          </tr>)}</tbody>
        </table>
      </div>}
  </Card>;
}

function EmailDelivery({ data }: { data: Overview }) {
  const total = data.email_delivery.reduce((sum, row) => sum + row.attempted, 0);
  return <Card>
    <CardHeader title={`近 ${data.range_days} 天邮件投递`} sub={`供应商调用 ${formatNumber(total)} 次`} />
    {data.email_delivery.length === 0
      ? <Empty title="暂无发信记录" className="py-8" />
      : <div className="overflow-x-auto">
        <table className="w-full border-collapse text-left text-[13px]">
          <thead><tr className="border-b border-hair text-muted"><th className="py-2 pr-4 font-medium">日期</th><th className="py-2 pr-4 font-medium">调用</th><th className="py-2 pr-4 font-medium">已接受</th><th className="py-2 pr-4 font-medium">拒收</th><th className="py-2 font-medium">失败</th></tr></thead>
          <tbody>{data.email_delivery.slice().reverse().map(row => <tr key={row.day} className="border-b border-hair last:border-b-0"><td className="py-2 pr-4 tabular-nums text-ink">{row.day}</td><td className="py-2 pr-4 tabular-nums text-ink">{formatNumber(row.attempted)}</td><td className="py-2 pr-4 tabular-nums text-ink">{formatNumber(row.accepted)}</td><td className="py-2 pr-4 tabular-nums text-ink">{formatNumber(row.rejected)}</td><td className="py-2 tabular-nums text-ink">{formatNumber(row.failed)}</td></tr>)}</tbody>
        </table>
      </div>}
  </Card>;
}

function TodoRow({ to, icon, title, sub, count }: { to: "/dictpr" | "/issues" | "/crash"; icon: string; title: string; sub: ReactNode; count: number | undefined }) {
  return <li>
    <Link to={to} search={{}} className="flex items-center gap-3 rounded-xl px-2.5 py-3 text-body no-underline transition hover:bg-panel-2 hover:text-body">
      <span aria-hidden="true" className="grid h-[34px] w-[34px] shrink-0 place-items-center rounded-[10px] bg-accent-soft font-bold text-accent-ink">{icon}</span>
      <span className="min-w-0 flex-1">
        <span className="block truncate font-semibold text-ink">{title}</span>
        <span className="block truncate text-[12.5px] text-muted">{sub}</span>
      </span>
      <span className="text-lg font-bold text-ink tabular-nums">{count === undefined ? "—" : formatNumber(count)}</span>
    </Link>
  </li>;
}

function Todos({ data }: { data: Overview }) {
  const pending = useShell().data?.pending;
  const community = useCommunityCounts();
  return <Card>
    <CardHeader title="待处理" />
    <ul className="-mx-2.5 -mb-2 m-0 list-none p-0">
      <TodoRow to="/dictpr" icon="词" title="词库 PR 待审核" sub="官网词库投稿生成的 GitHub PR" count={pending?.dict_prs} />
      <li className="px-2.5 py-3">
        <div className="flex items-center justify-between gap-3">
          <span className="font-semibold text-ink">社区内容待审核</span>
          <span className="text-lg font-bold text-ink tabular-nums">{formatNumber(pending?.community ?? data.pending.community)}</span>
        </div>
        <p className="m-0 mb-2 text-[12.5px] text-muted">{data.pending.reports_7d > 0 ? `近 7 天收到 ${formatNumber(data.pending.reports_7d)} 次举报` : "先发后审，新内容已公开展示"}</p>
        <nav className="grid grid-cols-2 gap-1" aria-label="社区待审核分类">
          {communityNavItems.map(item => <Link key={item.key} to={item.path} search={{ status: "pending" }}
            className="flex min-w-0 items-center justify-between gap-2 rounded-lg px-2 py-2 text-[12.5px] text-body no-underline hover:bg-panel-2">
            <span className="truncate">{item.label}</span>
            <span className="text-muted tabular-nums">{community.data && item.communityBadge ? formatNumber(community.data[item.communityBadge].pending) : "—"}</span>
          </Link>)}
        </nav>
      </li>
      <TodoRow to="/issues" icon="议" title="Issue 待分诊" sub="尚未分类的 GitHub Issue" count={pending?.issues} />
      <TodoRow to="/crash" icon="崩" title="新增崩溃分组" sub={data.crash_group_latest ? `${platformName(data.crash_group_latest.platform)} · ${data.crash_group_latest.title}` : "近 7 天首次出现、尚未处理"} count={data.pending.crash_groups} />
    </ul>
  </Card>;
}

function ServiceRow({ service }: { service: OverviewService }) {
  return <li className="flex items-center gap-2.5 border-b border-hair py-3 last:border-b-0">
    <Dot tone={stateTone[service.state]} />
    <span className="sr-only">{stateLabel[service.state]}</span>
    <span className="min-w-0 flex-1 truncate text-ink" title={service.provider ? `${service.name} · ${service.provider}` : service.name}>{service.name}</span>
    <span className="shrink-0 text-[12.5px] text-muted tabular-nums" title="近 60 天可用率 · 最近 P95 延迟">
      {service.uptime_60d === null && service.p95_ms === null ? "暂无数据" : `${service.uptime_60d === null ? "—" : `${service.uptime_60d.toFixed(2)}%`} · ${latency(service.p95_ms)}`}
    </span>
  </li>;
}

function Services({ data }: { data: Overview }) {
  return <Card>
    <CardHeader title="服务状态" actions={<Link to="/status" className="text-[13px] text-accent-ink no-underline hover:underline">查看详情</Link>} />
    {data.services.length === 0
      ? <Empty title={data.services_configured ? "暂无探测数据" : "未配置云服务"} className="py-8">
        {data.services_configured ? "状态探测运行后这里会显示每个服务的可用率与延迟。" : "在 config.json 的 admin.services 中配置服务与额度。"}
      </Empty>
      : <ul className="m-0 list-none p-0">{data.services.map(service => <ServiceRow key={service.key} service={service} />)}</ul>}
  </Card>;
}

function Loading() {
  return <div className="grid gap-3.5" role="status" aria-label="正在加载">
    <div className="grid grid-cols-2 gap-3.5 min-[1100px]:grid-cols-4">
      {["a", "b", "c", "d"].map(key => <Skeleton key={key} className="h-[104px] rounded-[18px]" />)}
    </div>
    <Card><SkeletonRows rows={5} className="p-0" /></Card>
  </div>;
}

export default function OverviewPage() {
  const api = useAPI();
  const query = useQuery({
    queryKey: keys.page("overview", { days: 30 }),
    queryFn: ({ signal }) => api.get("overview?days=30", overviewSchema, { signal }),
    refetchInterval: 60_000,
  });
  return <>
    <PageIntro page="overview" />
    {query.isPending && <Loading />}
    {query.isError && <ErrorState error={query.error} onRetry={() => query.refetch()} />}
    {query.data && <div className="grid gap-3.5">
      <KPIs data={query.data} />
      <div className="grid gap-3.5 min-[1100px]:grid-cols-[minmax(0,2fr)_minmax(0,1fr)]">
        <ActiveDevices data={query.data} />
        <PlatformActive data={query.data} />
      </div>
      <CrashPlatforms data={query.data} />
      <div className="grid gap-3.5 min-[820px]:grid-cols-2">
        <Todos data={query.data} />
        <Services data={query.data} />
      </div>
      <EmailDelivery data={query.data} />
    </div>}
  </>;
}

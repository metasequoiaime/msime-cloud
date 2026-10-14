import { useQuery } from "@tanstack/react-query";
import { differenceInMinutes, format, isToday, parseISO } from "date-fns";
import { useEffect, useRef } from "react";
import { useAPI } from "../../api/client";
import { formatLatency, formatRate, stateTone } from "../../api/cloud";
import { relativeTime } from "../../utils/time";
import { keys } from "../../api/keys";
import type { Incident, Status, StatusService } from "../../api/status";
import { statusSchema } from "../../api/status";
import { PageIntro } from "../../shell/page-intro";
import { usePageSearch } from "../../shell/page-search";
import { cn } from "../../ui/cn";
import { Dot, Pill } from "../../ui/pill";
import { Empty, ErrorState, Skeleton, SkeletonRows } from "../../ui/states";

// summary phrases the banner from the latest probe: how many services are fine and which ones are degraded or down.
function summary(status: Status): { tone: "ok" | "warn" | "bad" | "mute"; title: string } {
  const services = status.services;
  if (!status.checked_at) return { tone: "mute", title: "状态检查尚未运行，等待第一次探测" };
  // The server reports unknown once the last probe is more than three minutes old: the per-service states below are stale, so they are not summarised as current.
  if (status.state === "unknown") return { tone: "warn", title: `状态检查已停止，最近一次在${relativeTime(status.checked_at)}` };
  const degraded = services.filter(s => s.state === "degraded").map(s => s.name);
  const down = services.filter(s => s.state === "down").map(s => s.name);
  const healthy = services.length - degraded.length - down.length;
  if (!degraded.length && !down.length) return { tone: "ok", title: `全部 ${services.length} 项服务运行正常` };
  const parts = [`${healthy} 项服务正常`];
  if (down.length) parts.push(`${down.join("、")}不可用`);
  if (degraded.length) parts.push(`${degraded.join("、")}响应变慢或出错`);
  return { tone: status.state === "down" || down.length ? "bad" : "warn", title: parts.join("，") };
}

const bannerTone = { ok: "bg-accent-soft", warn: "bg-warn-soft", bad: "bg-bad-soft", mute: "bg-panel-2" } as const;
const bannerDot = { ok: "bg-accent ring-accent-soft", warn: "bg-warn ring-warn-soft", bad: "bg-bad ring-bad-soft", mute: "bg-muted ring-panel-2" } as const;

const dayColor = { ok: "bg-accent opacity-[.55]", degraded: "bg-warn", down: "bg-bad", none: "bg-panel-2" } as const;
const dayLabel = { ok: "正常", degraded: "响应变慢或出错", down: "有不可用时段", none: "无数据" } as const;

function ServiceRow({ service }: { service: StatusService }) {
  return <div className="px-5 py-3.5 shadow-[inset_0_-1px_0_var(--hair)] last:shadow-none">
    <div className="flex flex-wrap items-center gap-x-2.5 gap-y-1">
      <Dot tone={stateTone(service.state)} />
      <span className="font-semibold text-ink">{service.name}</span>
      {service.desc && <span className="text-[12.5px] text-muted">{service.desc}</span>}
      <span className="ml-auto text-[12.5px] text-muted tabular-nums">P95 {formatLatency(service.p95_ms)} · 可用 {formatRate(service.uptime_60d)}</span>
    </div>
    <div className="mt-2.5 flex h-[22px] gap-0.5" role="img" aria-label={`${service.name}近 60 天可用性`}>
      {service.days.map(day => <span key={day.day} title={`${day.day} · ${dayLabel[day.state]}${day.uptime != null ? ` · 可用 ${formatRate(day.uptime)}` : ""}`} className={cn("min-w-0.5 flex-1 rounded-[2px]", dayColor[day.state])} />)}
    </div>
  </div>;
}

function duration(from: Date, to: Date): string {
  const minutes = Math.max(1, differenceInMinutes(to, from));
  if (minutes < 60) return `${minutes} 分钟`;
  const hours = Math.floor(minutes / 60);
  return minutes % 60 ? `${hours} 小时 ${minutes % 60} 分钟` : `${hours} 小时`;
}

function when(date: Date): string {
  return isToday(date) ? `今天 ${format(date, "HH:mm")}` : format(date, "MM-dd HH:mm");
}

function incidentMeta(incident: Incident): string {
  const started = parseISO(incident.started_at);
  const source = incident.auto ? "自动检测" : "人工记录";
  if (incident.state === "open") return `进行中 · ${when(started)} 开始 · 已持续 ${duration(started, new Date())} · ${incident.service_name} · ${source}`;
  const resolved = incident.resolved_at ? parseISO(incident.resolved_at) : started;
  return `已解决 · ${format(started, "MM-dd")} · 持续 ${duration(started, resolved)} · ${incident.service_name} · ${source}`;
}

function IncidentRow({ incident, focused }: { incident: Incident; focused: boolean }) {
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (focused) ref.current?.scrollIntoView({ block: "center", behavior: "smooth" });
  }, [focused]);
  const open = incident.state === "open";
  return <div ref={ref} className={cn("flex gap-3 px-5 py-3.5 shadow-[inset_0_-1px_0_var(--hair)] last:shadow-none", focused && "bg-accent-soft")}>
    <Pill tone={open ? "warn" : "ok"}>{open ? "进行中" : "已解决"}</Pill>
    <div className="min-w-0">
      <div className="font-semibold text-ink">{incident.title}</div>
      {incident.description && <div className="mt-0.5 whitespace-pre-line text-[13px] text-body">{incident.description}</div>}
      <div className="mt-1 text-xs text-muted">{incidentMeta(incident)}</div>
    </div>
  </div>;
}

export default function StatusPage() {
  const api = useAPI();
  const { focus } = usePageSearch();
  const query = useQuery({
    queryKey: keys.page("status", "summary"),
    queryFn: ({ signal }) => api.get("status", statusSchema, { signal }),
    refetchInterval: 60_000,
  });
  const data = query.data;
  if (query.isError) return <><PageIntro page="status" /><ErrorState error={query.error} onRetry={() => query.refetch()} /></>;
  const banner = data ? summary(data) : null;
  return <>
    <PageIntro page="status" />
    <div className="flex flex-col gap-3.5">
      {banner && data
        ? <div className={cn("flex items-center gap-3 rounded-[18px] px-5 py-4", bannerTone[banner.tone])} role="status">
          <span aria-hidden="true" className={cn("h-2.5 w-2.5 shrink-0 rounded-full ring-4", bannerDot[banner.tone])} />
          <div className="min-w-0 flex-1">
            <div className="font-bold text-ink">{banner.title}</div>
            <div className="mt-0.5 text-[12.5px] text-body">本地输入不受影响，云端功能异常时键盘仍可使用本地候选。{data.checked_at ? `${relativeTime(data.checked_at)}检查` : "尚未检查"}</div>
          </div>
        </div>
        : <Skeleton className="h-[74px] rounded-[18px]" />}
      <section className="overflow-hidden rounded-[18px] bg-panel ring-1 ring-hair" aria-label="服务可用性">
        <div className="flex items-center px-5 py-4 shadow-[inset_0_-1px_0_var(--hair)]">
          <h2 className="m-0 text-[15px] font-bold text-ink">服务可用性</h2>
          <span className="ml-auto text-xs text-muted">近 60 天 · UTC</span>
        </div>
        {data ? data.services.map(service => <ServiceRow key={service.key} service={service} />) : <SkeletonRows rows={4} />}
      </section>
      <section className="overflow-hidden rounded-[18px] bg-panel ring-1 ring-hair" aria-label="近期事件">
        <div className="px-5 py-4 shadow-[inset_0_-1px_0_var(--hair)]">
          <h2 className="m-0 text-[15px] font-bold text-ink">近期事件</h2>
        </div>
        {!data && <SkeletonRows rows={3} />}
        {data && (data.incidents.length
          ? data.incidents.map(incident => <IncidentRow key={incident.id} incident={incident} focused={focus === String(incident.id)} />)
          : <Empty title="近期没有事件">服务连续 3 分钟变慢或出错时会自动记录事件，恢复 5 分钟后自动关闭。</Empty>)}
      </section>
    </div>
  </>;
}

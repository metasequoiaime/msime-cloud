import { useQuery } from "@tanstack/react-query";
import { useAPI } from "../../api/client";
import type { CloudService } from "../../api/cloud";
import { cloudSchema, formatLatency, formatRate, stateTone } from "../../api/cloud";
import { relativeTime } from "../../utils/time";
import { keys } from "../../api/keys";
import { PageIntro } from "../../shell/page-intro";
import { Banner } from "../../ui/card";
import { ChartEmpty, Sparkline } from "../../ui/charts";
import { Dot } from "../../ui/pill";
import { ProgressBar } from "../../ui/progress";
import { ErrorState, NotConfigured, Skeleton } from "../../ui/states";

const number = (value: number, digits = 0) => value.toLocaleString("zh-CN", { maximumFractionDigits: digits });

// Large character counts read as 万 like the design (190 万 / 500 万字符).
function chars(value: number): string {
  return value >= 10000 ? `${number(value / 10000, value >= 100000 ? 0 : 1)} 万` : `${number(value)} `;
}

function hours(value: number): string {
  return number(value, value < 10 ? 1 : 0);
}

// quotaText is the right-hand side of the 本月额度 row: percentage, then used / limit in the quota's unit; without a quota, the month's usage and the cost estimate when a unit price is configured.
function quotaText(service: CloudService): string {
  const { quota, month, cost_cny: cost } = service;
  if (!quota) {
    const usage = month.meter === "chars" ? `${chars(month.usage)}字符` : month.meter === "seconds" ? `${hours(month.usage / 3600)} 小时` : `${number(month.calls)} 次`;
    return `未设额度 · 本月 ${usage}${cost != null ? ` · 约 ¥ ${number(cost, 2)}` : ""}`;
  }
  const pct = `${number(quota.pct, quota.pct < 10 ? 1 : 0)}%`;
  switch (quota.unit) {
    case "cny":
      return `${pct} · ¥ ${number(quota.used)} / ${number(quota.limit)}`;
    case "chars":
      return `${pct} · ${chars(quota.used).trimEnd()} / ${chars(quota.limit)}字符`;
    case "hours":
      return `${pct} · ${hours(quota.used)} / ${number(quota.limit)} 小时`;
    default:
      return `${pct} · ${number(quota.used)} / ${number(quota.limit)} 次`;
  }
}

// Quotas past 60% are flagged like the design; an exhausted quota is bad.
function quotaTone(pct: number): "accent" | "warn" | "bad" {
  if (pct >= 100) return "bad";
  return pct >= 60 ? "warn" : "accent";
}

const stateHints = { ok: "运行正常", idle: "近 5 分钟无调用", degraded: "响应变慢或错误率升高", down: "不可用", unknown: "尚未检查" } as const;

function ServiceCard({ service }: { service: CloudService }) {
  const quotaPct = service.quota?.pct ?? 0;
  const quota = quotaText(service);
  return <div className="min-w-0 rounded-[18px] bg-panel px-5 py-[18px] ring-1 ring-hair">
    <div className="flex items-center gap-2">
      <span title={stateHints[service.state]} className="inline-flex"><Dot tone={stateTone(service.state)} /></span>
      <span className="truncate font-bold text-ink">{service.name}</span>
      <span className="ml-auto truncate text-xs text-muted" title={service.provider}>{service.provider}</span>
    </div>
    <div className="mt-2.5 text-[26px] font-bold leading-tight text-ink tabular-nums">{number(service.calls_24h)}</div>
    <div className="text-[12.5px] text-muted">近 24 小时调用 · P95 {formatLatency(service.p95_ms)} · 错误率 {formatRate(service.error_rate)}</div>
    <div className="mt-2.5">
      {service.calls_24h > 0
        ? <Sparkline data={service.hourly.map(hour => hour.calls)} ariaLabel={`${service.name}近 24 小时每小时调用量`} />
        : <ChartEmpty height={44}>近 24 小时无调用</ChartEmpty>}
    </div>
    <div className="mt-2.5">
      <div className="flex justify-between gap-3 text-xs text-muted">
        <span className="shrink-0">本月额度</span>
        <span className="truncate tabular-nums" title={quota}>{quota}</span>
      </div>
      <ProgressBar className="mt-[5px]" size="sm" value={Math.min(quotaPct, 100)} max={100} tone={quotaTone(quotaPct)} label={`${service.name}本月额度`} />
    </div>
  </div>;
}

export default function CloudPage() {
  const api = useAPI();
  const query = useQuery({
    queryKey: keys.page("cloud", "usage"),
    queryFn: ({ signal }) => api.get("cloud", cloudSchema, { signal }),
    refetchInterval: 60_000,
  });
  const data = query.data;
  return <>
    <PageIntro page="cloud">{data && <span className="text-xs text-muted">{relativeTime(data.generated_at)}更新 · 每分钟刷新</span>}</PageIntro>
    {query.isPending && <div className="grid grid-cols-[repeat(auto-fit,minmax(220px,1fr))] gap-3.5" role="status" aria-label="正在加载">
      {["a", "b", "c", "d"].map(key => <Skeleton key={key} className="h-[232px] rounded-[18px]" />)}
    </div>}
    {query.isError && <ErrorState error={query.error} onRetry={() => query.refetch()} />}
    {data && (data.services.length === 0
      ? <NotConfigured title="未配置云服务">服务端没有配置任何上游接口，也没有在 config.json 的 admin.services 中声明服务与额度。配置后这里会显示真实的调用量、延迟和额度。</NotConfigured>
      : <div className="grid grid-cols-[repeat(auto-fit,minmax(220px,1fr))] gap-3.5">
        {data.services.map(service => <ServiceCard key={service.key} service={service} />)}
      </div>)}
    <Banner className="mt-3.5">隐私边界：这里只统计调用次数、延迟和错误码，不记录任何拼音、候选或翻译内容。调用量为近 24 小时，额度按 UTC 自然月统计，金额是按配置单价的估算。</Banner>
  </>;
}

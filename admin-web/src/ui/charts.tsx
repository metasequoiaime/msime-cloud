import { formatNumber } from "../utils/number";
import type { ReactNode } from "react";
import { Area, AreaChart as RechartsAreaChart, CartesianGrid, Line, LineChart, ResponsiveContainer, Tooltip, XAxis, YAxis } from "recharts";
import { cn } from "./cn";

// Series colors follow the prototype: the theme accent first, then the two fixed overview colors.
export const chartColors = ["var(--chart-1)", "var(--chart-2)", "var(--chart-3)", "var(--info)", "var(--warn)"] as const;

export type ChartSeries<K extends string> = { key: K; label: string; color?: string };

type Datum = Record<string, string | number | null>;

const axisTick = { fill: "var(--muted)", fontSize: 12 };

function TooltipCard({ active, label, payload, formatValue, formatLabel }: { active?: boolean; label?: string | number; payload?: readonly { name?: string | number; value?: unknown; color?: string }[]; formatValue: (value: number) => string; formatLabel?: (label: string) => string }) {
  if (!active || !payload?.length) return null;
  return <div className="rounded-xl bg-panel px-3 py-2 text-xs shadow-pop">
    <div className="mb-1 font-semibold text-ink">{formatLabel ? formatLabel(String(label ?? "")) : label}</div>
    {payload.map(entry => <div key={String(entry.name)} className="flex items-center gap-2 text-body">
      <span className="h-2 w-2 rounded-full" style={{ background: entry.color }} />
      <span className="flex-1">{entry.name}</span>
      <span className="font-semibold text-ink tabular-nums">{typeof entry.value === "number" ? formatValue(entry.value) : String(entry.value ?? "—")}</span>
    </div>)}
  </div>;
}

export type AreaChartProps<K extends string> = {
  data: readonly Datum[];
  xKey: string;
  series: readonly ChartSeries<K>[];
  height?: number;
  // filled draws translucent areas; false draws plain 2.4px lines as on the overview.
  filled?: boolean;
  stacked?: boolean;
  yDomain?: [number, number];
  showYAxis?: boolean;
  formatX?: (value: string) => string;
  formatValue?: (value: number) => string;
  ariaLabel: string;
  className?: string;
};

// 图表回调还会传入刻度下标，只把数值交给公共格式化工具。
const defaultFormat = (value: number) => formatNumber(value);

// AreaChart is the multi-series time chart: dashed gridlines, muted axis labels, themed tooltip.
export function AreaChart<K extends string>({ data, xKey, series, height = 220, filled = true, stacked = false, yDomain, showYAxis = false, formatX, formatValue = defaultFormat, ariaLabel, className }: AreaChartProps<K>) {
  const color = (entry: ChartSeries<K>, index: number) => entry.color ?? chartColors[index % chartColors.length];
  const common = {
    data: data as Datum[],
    margin: { top: 8, right: 4, bottom: 0, left: showYAxis ? 0 : 4 },
  };
  const grid = <CartesianGrid vertical={false} stroke="var(--hair-2)" strokeDasharray="4 5" />;
  const xAxis = <XAxis dataKey={xKey} tick={axisTick} tickLine={false} axisLine={false} tickFormatter={formatX} minTickGap={24} />;
  const yAxis = <YAxis hide={!showYAxis} domain={yDomain ?? [0, "auto"]} tick={axisTick} tickLine={false} axisLine={false} width={44} tickFormatter={formatValue} />;
  const tooltip = <Tooltip cursor={{ stroke: "var(--hair-2)" }} content={props => <TooltipCard active={props.active} label={props.label as string | number | undefined} payload={props.payload as never} formatValue={formatValue} formatLabel={formatX} />} />;
  return <div role="img" aria-label={ariaLabel} className={cn("w-full min-w-0", className)}>
    <ResponsiveContainer width="100%" height={height}>
      {filled
        ? <RechartsAreaChart {...common}>{grid}{xAxis}{yAxis}{tooltip}{series.map((entry, index) => <Area key={entry.key} type="monotone" dataKey={entry.key} name={entry.label} stackId={stacked ? "stack" : undefined} stroke={color(entry, index)} fill={color(entry, index)} fillOpacity={0.12} strokeWidth={2} isAnimationActive={false} />)}</RechartsAreaChart>
        : <LineChart {...common}>{grid}{xAxis}{yAxis}{tooltip}{series.map((entry, index) => <Line key={entry.key} type="monotone" dataKey={entry.key} name={entry.label} stroke={color(entry, index)} strokeWidth={2.4} strokeLinejoin="round" strokeLinecap="round" dot={false} isAnimationActive={false} />)}</LineChart>}
    </ResponsiveContainer>
  </div>;
}

// ChartLegend is the dot + label row placed in a card header next to a chart.
export function ChartLegend<K extends string>({ series, className }: { series: readonly ChartSeries<K>[]; className?: string }) {
  return <div className={cn("flex flex-wrap items-center gap-3 text-[12.5px] text-muted", className)}>
    {series.map((entry, index) => <span key={entry.key} className="inline-flex items-center gap-1.5">
      <span className="h-2 w-2 rounded-full" style={{ background: entry.color ?? chartColors[index % chartColors.length] }} />
      {entry.label}
    </span>)}
  </div>;
}

// Sparkline is the 44px single-line trend used on service cards.
export function Sparkline({ data, color = "var(--accent)", height = 44, ariaLabel, className }: { data: readonly number[]; color?: string; height?: number; ariaLabel: string; className?: string }) {
  const points = data.map((value, index) => ({ index, value }));
  return <div role="img" aria-label={ariaLabel} className={cn("w-full min-w-0", className)}>
    <ResponsiveContainer width="100%" height={height}>
      <LineChart data={points} margin={{ top: 2, right: 0, bottom: 2, left: 0 }}>
        <YAxis hide domain={["dataMin", "dataMax"]} />
        <Line type="monotone" dataKey="value" stroke={color} strokeWidth={2} dot={false} isAnimationActive={false} />
      </LineChart>
    </ResponsiveContainer>
  </div>;
}

// ChartEmpty keeps the chart's height when there is no data (for example 客户端未上报).
export function ChartEmpty({ height = 220, children }: { height?: number; children: ReactNode }) {
  return <div className="grid place-items-center rounded-xl bg-panel-2 text-[13px] text-muted" style={{ height }}>{children}</div>;
}

import { formatNumber } from "../utils/number";
import { cn } from "./cn";
import { ProgressBar } from "./progress";

// 表格计数共用静态占比条；数值文本提供读屏内容，装饰条不重复播报进度。
export function CountBar({ value, max, tone = "accent", locale = "zh-CN", className, barClassName, valueClassName }: {
  value: number;
  max: number;
  tone?: "accent" | "warn" | "bad";
  locale?: "zh-CN" | "en-US";
  className?: string;
  barClassName?: string;
  valueClassName?: string;
}) {
  return <div className={cn("flex min-w-0 items-center gap-2.5", className)}>
    <div className="min-w-0 flex-1" aria-hidden="true">
      <ProgressBar value={value} max={max} tone={tone} size="sm" className={cn("[&>div]:transition-none", barClassName)} />
    </div>
    <span className={cn("min-w-10 text-right text-xs text-muted tabular-nums", valueClassName)}>{formatNumber(value, locale)}</span>
  </div>;
}

// 默认按小时显示时长；Issue 响应时间可选择把不足一小时的值显示为分钟，至少一分钟。
export function formatHours(value: number | null, subHourUnit: "hours" | "minutes" = "hours"): string {
  if (value === null) return "—";
  if (subHourUnit === "minutes" && value < 1) return `${Math.max(1, Math.round(value * 60))} 分钟`;
  return `${value < 10 ? value.toFixed(1) : Math.round(value)} 小时`;
}

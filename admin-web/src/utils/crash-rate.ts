import { formatNumber, formatRate } from "./number.ts";

// INSUFFICIENT 是会话数据不足时代替比率显示的文案；判定规则在服务端 internal/account/admin_crash_free.go。
export const INSUFFICIENT = "会话数据不足";

// sessionRateText 显示一个平台或全局的无崩溃会话率：有比率时显示百分比；有会话但所有版本都数据不足时显示「会话数据不足」；没有会话时显示破折号。
export function sessionRateText(rate: number | null | undefined, versions: number, insufficientVersions: number): string {
  if (rate != null) return formatRate(rate);
  return versions > 0 && insufficientVersions >= versions ? INSUFFICIENT : "—";
}

// excludedVersionsNote 说明有几个版本因会话数据不足没有计入比率；没有排除时返回空字符串。
export function excludedVersionsNote(insufficientVersions: number): string {
  return insufficientVersions > 0 ? `${formatNumber(insufficientVersions)} 个版本会话数据不足，未计入` : "";
}

// deviceRateSub 是崩溃设备占比下方的「崩溃设备 / 活跃设备」说明。
export function deviceRateSub(crashDevices: number, activeDevices: number): string {
  return `${formatNumber(crashDevices)} / ${formatNumber(activeDevices)} 台`;
}

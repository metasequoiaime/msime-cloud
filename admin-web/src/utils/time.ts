import { format, formatDistanceToNow } from "date-fns";
import { zhCN } from "date-fns/locale";

// 按本地时区显示日期；无效值保留原文，避免一条异常记录中断整个页面。
export function formatDateTime(value: string, pattern = "yyyy-MM-dd HH:mm"): string {
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? value : format(date, pattern, { locale: zhCN });
}

export function relativeTime(value: string): string {
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? value : formatDistanceToNow(date, { locale: zhCN, addSuffix: true });
}

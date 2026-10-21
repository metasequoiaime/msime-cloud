export function formatNumber(value: number, locale: "zh-CN" | "en-US" = "zh-CN", options?: Intl.NumberFormatOptions): string {
  return value.toLocaleString(locale, options);
}

// 把比率显示为百分比，默认保留两位小数；缺少数据时显示破折号。
export function formatRate(rate: number | null | undefined, digits = 2): string {
  return rate == null ? "—" : `${(rate * 100).toFixed(digits)}%`;
}

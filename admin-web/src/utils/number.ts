export function formatNumber(value: number, locale: "zh-CN" | "en-US" = "zh-CN", options?: Intl.NumberFormatOptions): string {
  return value.toLocaleString(locale, options);
}

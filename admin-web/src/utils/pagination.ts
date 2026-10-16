// 后台列表的 URL 页码按十进制整数前缀解析，缺省或无效时为 1，并限制在 1 至 10000。
export function parsePage(value: string | undefined): number {
  return Math.max(1, Math.min(10000, Number.parseInt(value ?? "1", 10) || 1));
}

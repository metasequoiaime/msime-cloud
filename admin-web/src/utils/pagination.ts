// 后台列表的 URL 页码按十进制整数前缀解析，缺省或无效时为 1；max 为 Infinity 时不设上限。
export function parsePage(value: string | undefined, max = 10000): number {
  return Math.max(1, Math.min(max, Number.parseInt(value ?? "1", 10) || 1));
}

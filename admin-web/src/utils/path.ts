// lastPathSegment 返回斜杠分隔字符串的最后一段。
export function lastPathSegment(value: string): string {
  return value.slice(value.lastIndexOf("/") + 1);
}

// firstLine 取文本的第一行并去掉首尾空白，供列表摘要复用。
export function firstLine(text: string): string {
  return text.split("\n")[0].trim();
}

// initial 取名称的首个 Unicode 字符，供头像复用。
export function initial(name: string): string {
  return (Array.from(name.trim())[0] ?? "?").toUpperCase();
}

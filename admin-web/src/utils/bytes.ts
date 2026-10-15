export type ByteUnit = "B" | "KB" | "MB";

export type SplitBytes = { value: number; unit: ByteUnit };

export function splitBytes(bytes: number): SplitBytes {
  if (bytes < 1024) return { value: bytes, unit: "B" };
  if (bytes < 1024 * 1024) return { value: bytes / 1024, unit: "KB" };
  return { value: bytes / 1024 / 1024, unit: "MB" };
}

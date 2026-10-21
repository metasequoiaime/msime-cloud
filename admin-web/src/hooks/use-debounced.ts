import { useCallback, useState } from "react";
import { useDebouncedEffect } from "./use-debounced-effect";

// 输入或延迟变化时重新计时；重新计时和卸载时清理上一次等待。
export function useDebounced<T>(value: T, ms: number): T {
  const [debounced, setDebounced] = useState(value);
  useDebouncedEffect(useCallback(() => setDebounced(value), [value]), ms);
  return debounced;
}

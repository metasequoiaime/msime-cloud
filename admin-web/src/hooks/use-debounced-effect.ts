import { useEffect } from "react";

// 回调身份、延迟或启用状态变化时重新计时；调用方用 useCallback 声明需要重置等待的依赖。
export function useDebouncedEffect(effect: () => void, ms: number, enabled = true): void {
  useEffect(() => {
    if (!enabled) return;
    const timer = window.setTimeout(effect, ms);
    return () => window.clearTimeout(timer);
  }, [effect, ms, enabled]);
}

// createListeners 为外部状态创建独立的订阅与同步通知实例。
export function createListeners() {
  const listeners = new Set<() => void>();
  return {
    subscribe(listener: () => void) {
      listeners.add(listener);
      return () => { listeners.delete(listener); };
    },
    emit() {
      for (const listener of listeners) listener();
    },
  };
}

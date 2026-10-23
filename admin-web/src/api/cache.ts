import type { QueryClient, QueryKey } from "@tanstack/react-query";

// 并行刷新指定查询前缀，返回的 Promise 等待所有活跃查询刷新完成。
export function invalidateQueries(client: QueryClient, ...queryKeys: QueryKey[]) {
  return Promise.all(queryKeys.map(queryKey => client.invalidateQueries({ queryKey })));
}

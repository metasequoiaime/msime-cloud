import { useQuery } from "@tanstack/react-query";
import { useAPI } from "../api/client";
import { countsSchema } from "../api/community";
import { keys } from "../api/keys";

// 各内容入口共用待审核计数缓存，审核后随 community 查询一起刷新。
export function useCommunityCounts() {
  const api = useAPI();
  return useQuery({
    queryKey: keys.page("community", "counts"),
    queryFn: ({ signal }) => api.get("community/counts", countsSchema, { signal }),
    refetchInterval: 60_000,
    staleTime: 30_000,
  });
}

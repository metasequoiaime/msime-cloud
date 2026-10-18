import { useQuery } from "@tanstack/react-query";
import { useAPI } from "../../api/client";
import { contentPreviewSchema } from "../../api/community";
import type { Item } from "../../api/community";
import { keys } from "../../api/keys";
import { SkinPreview } from "../../skin-preview";
import { Skeleton } from "../../ui/states";
import { usePreviewVisible } from "./use-preview-visible";

export function KeyboardCardPreview({ item }: { item: Item }) {
  const { container, visible } = usePreviewVisible();
  const api = useAPI();
  // 列表会移除 photo；进入视口后只读取完整设计，以还原照片背景。
  const design = item.design && typeof item.design === "object" ? item.design as Record<string, unknown> : null;
  const needsDetail = design?.photoShade !== undefined || design?.photoPosition !== undefined;
  const detail = useQuery({
    queryKey: keys.page("community", "keyboard-preview", item.id, item.edit_token ?? item.updated_at),
    enabled: visible && needsDetail,
    staleTime: 5 * 60_000,
    queryFn: ({ signal }) => api.get(`skins/${encodeURIComponent(item.id)}?view=preview`, contentPreviewSchema, { signal }),
  });
  return <div ref={container} className="h-full">
    {needsDetail && detail.isPending ? <Skeleton className="h-full w-full" /> : needsDetail && detail.isError
      ? <div className="grid h-full place-items-center bg-panel-2 text-[13px] text-muted">预览加载失败</div>
      : <SkinPreview content={needsDetail ? detail.data?.content : item.design} name={item.name} compact />}
  </div>;
}

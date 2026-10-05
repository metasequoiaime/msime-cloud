import { useContext } from "react";
import { useQuery } from "@tanstack/react-query";
import { APICredentialsContext, APIError } from "../../api/client";
import { platformName } from "../../api/crash";
import { diagnosticLabels, feedbackStatusLabels, feedbackStatusTones, feedbackTypeLabels } from "../../api/feedback";
import type { FeedbackRow } from "../../api/feedback";
import { keys } from "../../api/keys";
import { relativeTime } from "../../shell/notifications";
import { DetailDrawer } from "../../ui/drawer";
import type { DrawerAction, DrawerField, DrawerSection } from "../../ui/drawer";
import { Skeleton, SkeletonRows } from "../../ui/states";

// useScreenshot 经管理接口读一张截图并转成 data: URL（后台 CSP 的 img-src 只放行 'self' 和 data:），会话 Cookie 和内存里的旧令牌都能用。
function useScreenshot(id: string, position: number) {
  const credentials = useContext(APICredentialsContext);
  const token = credentials?.token;
  return useQuery({
    queryKey: keys.page("feedback", "screenshot", id, position),
    staleTime: 5 * 60_000,
    queryFn: async ({ signal }) => {
      const response = await fetch(`/api/feedback/${encodeURIComponent(id)}/screenshots/${position}`, { signal, credentials: "same-origin", headers: token ? { Authorization: `Bearer ${token}` } : {} });
      if (!response.ok) throw new APIError(response.status, "");
      const blob = await response.blob();
      return await new Promise<string>((resolve, reject) => {
        const reader = new FileReader();
        reader.onload = () => resolve(String(reader.result));
        reader.onerror = () => reject(reader.error);
        reader.readAsDataURL(blob);
      });
    },
  });
}

function Screenshot({ id, position }: { id: string; position: number }) {
  const image = useScreenshot(id, position);
  if (image.isPending) return <Skeleton className="h-[220px] rounded-[12px]" />;
  if (image.isError) return <p className="m-0 rounded-[12px] bg-panel-2 px-3 py-6 text-center text-[13px] text-muted">截图加载失败</p>;
  return <img src={image.data} alt={`截图 ${position + 1}`} className="block max-h-[360px] w-full rounded-[12px] bg-panel-2 object-contain" />;
}

// 每条反馈最多 3 张截图，位置 0–2 与服务端的 feedback_screenshots.position 一致。
const screenshotPositions = [0, 1, 2] as const;

// FeedbackDrawer 是从行或 ?focus=<id> 打开的反馈详情：全文、提交信息、诊断信息和截图。
export function FeedbackDrawer({ row, open, loading, canTriage, onClose, onStatus }: {
  row: FeedbackRow | undefined; open: boolean; loading: boolean; canTriage: boolean; onClose: () => void; onStatus: (row: FeedbackRow, resolved: boolean) => Promise<void>;
}) {
  const fields: DrawerField[] = row ? [
    { label: "提交者", value: row.anonymous ? `${row.author}（匿名账号）` : row.author },
    { label: "提交时间", value: relativeTime(row.created_at) },
    { label: "平台", value: platformName(row.platform) },
    { label: "App 版本", value: row.app_version || "—", mono: true },
    { label: "版本", value: row.edition || "—", mono: true },
    { label: "账号 ID", value: row.user_id, mono: true },
  ] : [];
  const diagnostics = Object.entries(row?.diagnostics ?? {});
  const sections: DrawerSection[] = row ? [
    { title: "反馈内容", content: <p className="m-0 rounded-[10px] bg-panel-2 px-3 py-2.5 text-[13.5px] leading-relaxed whitespace-pre-wrap break-words text-ink">{row.text}</p> },
    { title: "诊断信息", items: diagnostics.map(([key, value]) => ({ text: diagnosticLabels[key] ?? key, meta: <span className="font-mono">{value}</span> })), empty: "用户没有附带诊断信息" },
    ...(row.screenshots > 0 ? [{ title: `截图（${row.screenshots} 张）`, content: <div className="grid gap-3">{screenshotPositions.slice(0, row.screenshots).map(position => <Screenshot key={position} id={row.id} position={position} />)}</div> }] : []),
  ] : [];
  const actions: DrawerAction[] = row ? [row.status === "new"
    ? { label: "标记已处理", variant: "primary", disabled: !canTriage, onClick: () => void onStatus(row, true) }
    : { label: "重新打开", disabled: !canTriage, onClick: () => void onStatus(row, false) }] : [];

  return <DetailDrawer
    open={open}
    onClose={onClose}
    title={row ? `${feedbackTypeLabels[row.type]}反馈` : "反馈详情"}
    sub={row?.id}
    pills={row ? [{ text: feedbackStatusLabels[row.status], tone: feedbackStatusTones[row.status] }] : undefined}
    fields={fields}
    sections={sections}
    actions={actions}
  >
    {!row && (loading ? <SkeletonRows rows={4} /> : <p className="m-0 text-[13px] text-muted">这条反馈不在当前列表页，可能已被清理或不符合当前筛选条件。</p>)}
  </DetailDrawer>;
}

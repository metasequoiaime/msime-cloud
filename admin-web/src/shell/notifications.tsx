import { useState } from "react";
import { Popover } from "radix-ui";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { relativeTime } from "../utils/time";
import { Bell } from "lucide-react";
import { errorMessage, useAPI } from "../api/client";
import { keys } from "../api/keys";
import { notificationKindLabels, notificationsReadSchema, notificationsSchema } from "../api/shell";
import type { Notification } from "../api/shell";
import { pageForTarget } from "../nav";
import { Button } from "../ui/button";
import { cn } from "../ui/cn";
import { SkeletonRows } from "../ui/states";
import { useToast } from "../ui/toast";

// NotificationBell shows the unread count from /api/shell and loads the latest 20 notifications when opened.
export function NotificationBell({ unread }: { unread: number }) {
  const api = useAPI();
  const client = useQueryClient();
  const navigate = useNavigate();
  const toast = useToast();
  const [open, setOpen] = useState(false);
  const list = useQuery({
    queryKey: keys.notifications,
    queryFn: ({ signal }) => api.get("notifications?limit=20", notificationsSchema, { signal }),
    enabled: open,
    // Every opening refetches, so 全部已读 acts on what the admin is actually looking at.
    staleTime: 0,
  });
  const markRead = useMutation({
    mutationFn: (body: { ids: string[] } | { all: true; up_to_id: string }) => api.post("notifications/read", notificationsReadSchema, body),
    onSettled: () => Promise.all([client.invalidateQueries({ queryKey: keys.notifications }), client.invalidateQueries({ queryKey: keys.shell })]),
  });
  // While the popover is closed the cached list goes stale (and a read made from it settles after the query is disabled), so the badge follows the 60s /api/shell poll; the open popover shows the count that came with its list.
  const count = open && list.data ? list.data.unread : unread;
  // 全部已读 covers the newest notification on screen, not the server's now(), so one that arrived after the list was fetched stays unread.
  const newest = list.data?.items[0]?.id;
  const openItem = (item: Notification) => {
    setOpen(false);
    if (!item.read) markRead.mutate({ ids: [item.id] });
    void navigate({ to: pageForTarget(item.target_page, item.target_id ?? undefined), search: item.target_id ? { focus: item.target_id } : {} });
  };
  return <Popover.Root open={open} onOpenChange={setOpen}>
    <Popover.Trigger asChild>
      <Button variant="outline" size="icon" title="通知" aria-label={count ? `通知，${count} 条未读` : "通知"} className="relative">
        <Bell size={18} aria-hidden="true" />
        {count > 0 && <span className="absolute -top-1 -right-1 inline-flex h-4 min-w-4 items-center justify-center rounded-full bg-bad px-1 text-[10.5px] font-bold text-bad-fg tabular-nums">{count > 99 ? "99+" : count}</span>}
      </Button>
    </Popover.Trigger>
    <Popover.Portal>
      <Popover.Content align="end" sideOffset={8} collisionPadding={16} className="z-40 w-[340px] max-w-[calc(100vw-32px)] animate-pop-in rounded-2xl bg-panel p-2 shadow-pop outline-none">
        <div className="flex items-center justify-between px-2.5 pt-1.5 pb-2">
          <span className="font-bold text-ink">通知</span>
          <button type="button" className="text-[13px] text-accent-ink hover:underline disabled:opacity-45" disabled={count === 0 || !newest || markRead.isPending}
            onClick={() => newest && markRead.mutate({ all: true, up_to_id: newest }, { onSuccess: () => toast("已全部标记为已读"), onError: error => toast(`操作失败：${errorMessage(error)}`) })}>全部已读</button>
        </div>
        {list.isPending && <SkeletonRows rows={3} className="p-2" />}
        {list.isError && <p className="m-0 px-2.5 py-3 text-[13px] text-bad" role="alert">{errorMessage(list.error)}</p>}
        {list.data && list.data.items.length === 0 && <p className="m-0 px-2.5 py-6 text-center text-[13px] text-muted">暂无通知</p>}
        {list.data && list.data.items.length > 0 && <ul className="m-0 max-h-[420px] list-none overflow-y-auto p-0">
          {list.data.items.map(item => <li key={item.id}>
            <button type="button" onClick={() => openItem(item)} className="flex w-full items-start gap-2.5 rounded-[10px] px-2.5 py-2.5 text-left transition hover:bg-panel-2">
              <span aria-hidden="true" className={cn("mt-[7px] h-2 w-2 shrink-0 rounded-full", item.read ? "bg-transparent" : "bg-accent")} />
              <span className="min-w-0 flex-1">
                <span className={cn("block leading-normal text-ink", item.read ? "font-normal" : "font-semibold")}>{item.title}</span>
                <span className="block text-xs text-muted">{notificationKindLabels[item.kind] ?? item.kind} · {relativeTime(item.created_at)}</span>
              </span>
            </button>
          </li>)}
        </ul>}
      </Popover.Content>
    </Popover.Portal>
  </Popover.Root>;
}

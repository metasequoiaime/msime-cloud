import { useQuery } from "@tanstack/react-query";
import { useAPI } from "../../api/client";
import { keys } from "../../api/keys";
import { userDetailSchema } from "../../api/users";
import type { UserDetail } from "../../api/users";
import { noPermissionHint, usePermissions } from "../../shell/permissions";
import { formatDateTime, relativeTime } from "../../utils/time";
import { Button } from "../../ui/button";
import { DetailDrawer } from "../../ui/drawer";
import type { DrawerAction, DrawerField, DrawerItem } from "../../ui/drawer";
import { ErrorState, SkeletonRows } from "../../ui/states";
import type { useUserActions } from "./actions";
import { activeText, contactText, devicePlatform, displayName, historyText, providersText, roleOf, workMeta, workText } from "./labels";

type Actions = ReturnType<typeof useUserActions>;

// UserDrawer is the account detail opened from a row or from ?focus=<id>.
export function UserDrawer({ id, onClose, actions }: { id: string | undefined; onClose: () => void; actions: Actions }) {
  const api = useAPI();
  const { can } = usePermissions();
  const canBan = can("ban_users");
  const detail = useQuery({
    queryKey: keys.page("users", "detail", id),
    queryFn: ({ signal }) => api.get(`users/${encodeURIComponent(id ?? "")}`, userDetailSchema, { signal }),
    enabled: Boolean(id),
  });
  const user = detail.data;
  const name = user ? displayName(user) : "用户详情";
  const target = user ? { id: user.id, name } : undefined;

  const fields: DrawerField[] = user ? [
    { label: "账号 ID", value: user.id, mono: true },
    { label: "登录方式", value: providersText(user.providers) },
    { label: "注册时间", value: formatDateTime(user.created_at, "yyyy-MM-dd") },
    { label: "最近活跃", value: activeText(user.last_active) },
    { label: "设置同步", value: user.sync ? "已开启" : "未开启" },
    { label: "有效会话", value: `${user.active_sessions} / ${user.total_sessions}` },
    ...(user.banned ? [
      { label: "封禁原因", value: user.ban_reason || "—" },
      { label: "封禁时间", value: user.banned_at ? relativeTime(user.banned_at) : "—" },
    ] : []),
  ] : [];

  const drawerActions: DrawerAction[] = [];
  if (user && target) {
    if (user.banned) {
      drawerActions.push({ label: "解除封禁", variant: "primary", disabled: !canBan, onClick: () => void actions.unban(target) });
    } else {
      if (user.active_sessions > 0) drawerActions.push({ label: "下线全部设备", disabled: !canBan, onClick: () => void actions.revokeAll(target) });
      drawerActions.push({ label: "封禁", variant: "danger", disabled: !canBan, onClick: () => void actions.ban(target) });
    }
  }

  return <DetailDrawer
    open={Boolean(id)}
    onClose={onClose}
    title={name}
    sub={user ? contactText(user) : undefined}
    pills={user ? [{ text: roleOf(user.role).label, tone: roleOf(user.role).tone }, user.banned ? { text: "已封禁", tone: "bad" } : { text: "正常", tone: "ok" }] : undefined}
    fields={fields}
    sections={user ? [
      { title: "登录设备", content: <Devices user={user} canRevoke={canBan} onRevoke={session => void actions.revokeSession(user.id, session)} /> },
      { title: "社区作品", items: user.works.map(work => ({ text: workText(work), meta: workMeta(work) })), empty: "还没有发布作品" },
      { title: "最近记录", items: recentItems(user), empty: "暂无记录" },
    ] : undefined}
    actions={drawerActions}
  >
    {detail.isPending && Boolean(id) && <SkeletonRows rows={6} className="p-0" />}
    {detail.isError && <ErrorState error={detail.error} onRetry={() => void detail.refetch()} />}
  </DetailDrawer>;
}

// Devices lists the account's active sessions with the platform read from the recorded User-Agent; sessions created before User-Agents were recorded say so.
function Devices({ user, canRevoke, onRevoke }: { user: UserDetail; canRevoke: boolean; onRevoke: (session: string) => void }) {
  const active = user.sessions.filter(session => session.status === "active");
  if (active.length === 0) return <div className="rounded-[10px] bg-panel-2 px-3 py-2.5 text-[13.5px] text-muted">{user.banned ? "封禁时已下线全部设备" : "没有登录中的设备"}</div>;
  return <div className="grid gap-1.5">
    {active.map(session => <div key={session.id} className="flex items-center gap-3 rounded-[10px] bg-panel-2 px-3 py-2.5">
      <div className="min-w-0 flex-1">
        <div className="text-[13.5px] font-semibold text-ink">{devicePlatform(session.user_agent)}</div>
        <div className="mt-0.5 truncate text-xs text-muted" title={session.user_agent || undefined}>{session.user_agent || "未记录设备信息"}</div>
        <div className="mt-0.5 text-xs text-muted">登录于 {relativeTime(session.created_at)} · 最近活跃 {relativeTime(session.last_active)}</div>
      </div>
      <Button size="sm" variant="outline" disabled={!canRevoke} title={canRevoke ? undefined : noPermissionHint} onClick={() => onRevoke(session.id)}>下线</Button>
    </div>)}
  </div>;
}

// recentItems merges the admin actions on the account with its latest logins, newest first.
function recentItems(user: UserDetail): DrawerItem[] {
  const entries = [
    ...user.history.map(entry => ({ at: entry.created_at, ...historyText(entry) })),
    ...user.sessions.slice(0, 3).map(session => ({ at: session.created_at, text: `登录 · ${devicePlatform(session.user_agent)}`, meta: relativeTime(session.created_at) })),
  ];
  return entries.sort((a, b) => Date.parse(b.at) - Date.parse(a.at)).slice(0, 8).map(({ text, meta }) => ({ text, meta }));
}

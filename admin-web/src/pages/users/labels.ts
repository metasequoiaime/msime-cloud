import { format } from "date-fns";
import type { Tone } from "../../ui/pill";
import { relativeTime } from "../../shell/notifications";
import type { UserHistory, UserRow, UserWork } from "../../api/users";

// Role pills follow the design: maintainers in the accent tone, reviewers in info, everyone else muted.
export const userRoles: Record<string, { label: string; tone: Tone }> = {
  maintainer: { label: "维护者", tone: "ok" },
  reviewer: { label: "审核志愿者", tone: "info" },
  operator: { label: "运营/客服", tone: "accent" },
  readonly: { label: "只读", tone: "mute" },
  user: { label: "普通用户", tone: "mute" },
};
export function roleOf(role: string): { label: string; tone: Tone } {
  return userRoles[role] ?? { label: role, tone: "mute" };
}

export const banReasons = ["批量发布侵权内容", "发布广告导流", "恶意举报", "冒充官方"] as const;

// displayName mirrors the server's default name for accounts that never set one.
export function displayName(user: Pick<UserRow, "id" | "display_name">): string {
  return user.display_name.trim() || `水杉小鹿·${user.id.slice(0, 6).toUpperCase()}`;
}

export function contactText(user: Pick<UserRow, "contact" | "contact_kind">): string {
  if (!user.contact) return "未绑定联系方式";
  return user.contact_kind === "phone" ? `手机 ${user.contact}` : user.contact;
}

const providerLabels: Record<string, string> = { email: "邮箱", phone: "手机号", google: "Google", apple: "Apple", wechat: "微信", anonymous: "匿名口令" };
export function providersText(providers: readonly string[]): string {
  return providers.length ? providers.map(provider => providerLabels[provider] ?? provider).join("、") : "无";
}

export function activeText(value: string | null): string {
  return value ? relativeTime(value) : "从未登录";
}

export function dateText(value: string): string {
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? value : format(date, "yyyy-MM-dd");
}

// devicePlatform reads the platform out of the User-Agent recorded at login; the clients may only identify their platform.
export function devicePlatform(userAgent: string): string {
  const ua = userAgent.toLowerCase();
  if (!ua) return "未知设备";
  if (/harmonyos|openharmony/.test(ua)) return "HarmonyOS";
  if (/iphone|ipad|ios/.test(ua)) return "iOS";
  if (/android/.test(ua)) return "Android";
  if (/windows/.test(ua)) return "Windows";
  if (/mac ?os|macintosh|darwin/.test(ua)) return "macOS";
  if (/linux|x11/.test(ua)) return "Linux";
  return "其他设备";
}

const sectionLabels: Record<UserWork["section"], string> = { skins: "皮肤", "candidate-skins": "候选栏皮肤", plugins: "插件", dictionaries: "词库", replies: "回复模板", phrases: "短语包" };
const moderationLabels: Record<UserWork["moderation"], string> = { pending: "待审核", approved: "已通过", removed: "已下架" };

export function workText(work: UserWork): string {
  return `${sectionLabels[work.section]}「${work.name}」`;
}

export function workMeta(work: UserWork): string {
  const usage = work.section === "dictionaries" || work.section === "replies" || work.section === "phrases" ? `收藏 ${work.saves.toLocaleString("zh-CN")}` : `下载 ${work.downloads.toLocaleString("zh-CN")}`;
  let state: string = moderationLabels[work.moderation];
  if (work.moderation === "removed" && work.moderation_reason === "owner_banned") state = "已下架（账号封禁）";
  else if (work.moderation === "removed" && work.moderation_reason) state = `已下架（${work.moderation_reason}）`;
  return `${usage} · ${state} · ${relativeTime(work.created_at)}`;
}

// historyText phrases one audit entry about the account the way the permission log does.
export function historyText(entry: UserHistory): { text: string; meta: string } {
  const reason = typeof entry.detail.reason === "string" && entry.detail.reason ? ` · 原因：${entry.detail.reason}` : "";
  const when = relativeTime(entry.created_at);
  switch (entry.action) {
    case "ban_user": return { text: `被 ${entry.actor} 封禁`, meta: `${when}${reason}` };
    case "unban_user": return { text: `${entry.actor} 解除了封禁`, meta: `${when}${reason}` };
    case "revoke_sessions": return { text: `${entry.actor} 下线了全部设备`, meta: when };
    case "revoke_session": return { text: `${entry.actor} 下线了一台设备`, meta: when };
    default: return { text: `${entry.actor} · ${entry.action}`, meta: when };
  }
}

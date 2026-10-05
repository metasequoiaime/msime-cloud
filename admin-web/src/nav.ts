import type { Permission, Shell } from "./api/shell";

export type PageKey = "overview" | "dictpr" | "community" | "issues" | "feedback" | "words" | "users" | "downloads" | "notice" | "release" | "cloud" | "crash" | "status" | "logs" | "perm" | "me";

export type NavItem = {
  key: PageKey;
  label: string;
  path: string;
  description: string;
  // SVG path data on a 24x24 viewBox, taken from the design prototype.
  icon: string;
  badge?: keyof Shell["pending"];
  // permission hides the item from the sidebar and global search for roles without it, for pages whose reads are permission-gated on the server (GET /api/cloud requires view_cloud_usage).
  permission?: Permission;
  // feature 是页面依赖的可选功能（/api/shell 的 features），未在部署配置中启用时侧栏和全局搜索都不显示该页。
  feature?: keyof NonNullable<Shell["features"]>;
};

export const navGroups: readonly { title: string; items: readonly NavItem[] }[] = [
  { title: "概览", items: [
    { key: "overview", label: "数据概览", path: "/", description: "装机、活跃、崩溃与待办的整体情况。", icon: "M4 4h6v6H4zM14 4h6v6h-6zM4 14h6v6H4zM14 14h6v6h-6z" },
  ] },
  { title: "审核", items: [
    { key: "dictpr", label: "词库审核", path: "/dictpr", description: "审核官网词库投稿生成的 GitHub PR。", icon: "M5 19.5V4.5A1.5 1.5 0 0 1 6.5 3H19v15H6.5A1.5 1.5 0 0 0 5 19.5A1.5 1.5 0 0 0 6.5 21H19v-3M9 10l2 2 4-4", badge: "dict_prs" },
    { key: "community", label: "社区审核", path: "/community", description: "复核社区皮肤、候选皮肤、插件、词库、回复模板和短语包。", icon: "M8 3 4 6l2 4 2-1v12h8V9l2 1 2-4-4-3c-.5 1.5-2 2.5-4 2.5S8.5 4.5 8 3z", badge: "community" },
    { key: "issues", label: "问题分诊", path: "/issues", description: "分类、指派和回复 GitHub Issue。", icon: "M12 3a9 9 0 1 0 0 18 9 9 0 0 0 0-18zM12 8v5M12 16.5v.01", badge: "issues" },
    { key: "feedback", label: "用户反馈", path: "/feedback", description: "用户在 App 里提交的问题、建议和词库反馈，以及附带的截图。", icon: "M4 5h16v11H8l-4 4zM8 9h8M8 12.5h5" },
    { key: "words", label: "敏感词库", path: "/words", description: "词库投稿和社区内容的敏感词名单。", icon: "M12 3a9 9 0 1 0 0 18 9 9 0 0 0 0-18zM5.6 5.6l12.8 12.8" },
  ] },
  { title: "运营", items: [
    { key: "users", label: "用户账号", path: "/users", description: "注册用户、登录会话与封禁。", icon: "M9 11a3.5 3.5 0 1 0 0-7 3.5 3.5 0 0 0 0 7zM3 20c0-3.3 2.7-6 6-6s6 2.7 6 6M16 4.5a3.5 3.5 0 0 1 0 6.5M18 14.5c1.8.8 3 2.8 3 5.5" },
    { key: "downloads", label: "下载记录", path: "/downloads", description: "安装包下载上报与 GitHub Release 下载量。", icon: "M12 4v11M7.5 10.5 12 15l4.5-4.5M5 19h14" },
    { key: "notice", label: "公告推送", path: "/notice", description: "编写并发布面向用户的公告。", icon: "M4 10v4h3l7 4V6l-7 4zM17.5 9a4 4 0 0 1 0 6" },
  ] },
  { title: "工程", items: [
    { key: "release", label: "发布管理", path: "/release", description: "各平台版本、发布说明和发布流水线。", icon: "M12 3 4 7v10l8 4 8-4V7zM4 7l8 4 8-4M12 11v10" },
    { key: "cloud", label: "云端监控", path: "/cloud", description: "云端服务的调用量、延迟、错误率和额度。", icon: "M7 18a4.5 4.5 0 0 1-.6-8.96A6 6 0 0 1 18 9.5a4.25 4.25 0 0 1-.5 8.5z", permission: "view_cloud_usage" },
    { key: "crash", label: "崩溃上报", path: "/crash", description: "按签名分组的客户端崩溃。", icon: "M13 3 5 13.5h6L10 21l8-10.5h-6z" },
  ] },
  { title: "系统", items: [
    { key: "status", label: "系统状态", path: "/status", description: "后端各服务的可用性与事件。", icon: "M3 12h4l2.5-6 5 12 2.5-6h4" },
    { key: "logs", label: "服务日志", path: "/logs", description: "后端各副本的实时日志，来自集群的 Loki。", icon: "M4 5h16M4 10h10M4 15h16M4 20h7", permission: "view_logs", feature: "logs" },
    { key: "perm", label: "权限日志", path: "/perm", description: "管理员角色、权限矩阵与操作日志。", icon: "M12 3 5 6v5c0 4.5 3 8.5 7 10 4-1.5 7-5.5 7-10V6zM9 12l2 2 4-4" },
  ] },
];

export const meItem: NavItem = { key: "me", label: "个人中心", path: "/me", description: "个人资料、通知偏好、会话与访问令牌。", icon: "M12 12a4 4 0 1 0 0-8 4 4 0 0 0 0 8zM4 21c0-4 3.6-7 8-7s8 3 8 7" };

export const navItems: readonly NavItem[] = [...navGroups.flatMap(group => group.items), meItem];

export function navItem(key: PageKey): NavItem {
  const item = navItems.find(entry => entry.key === key);
  if (!item) throw new Error(`unknown page ${key}`);
  return item;
}

// Breadcrumb group for a page; the profile page sits outside the nav under 账号.
export function navGroupTitle(key: PageKey): string {
  return navGroups.find(group => group.items.some(item => item.key === key))?.title ?? "账号";
}

export function pageForPath(pathname: string): NavItem | undefined {
  const path = pathname.length > 1 ? pathname.replace(/\/+$/, "") : pathname;
  return navItems.find(item => item.path === path);
}

// Old routes kept as redirects so bookmarks keep working.
export const legacyRedirects: Readonly<Record<string, string>> = {
  "/admins": "/perm",
  "/audit": "/perm",
  "/system": "/status",
  "/crashes": "/crash",
  "/skins": "/community",
  "/dictionaries": "/community",
  "/replies": "/community",
  "/site-settings": "/downloads",
};

// pageForTarget turns a server-side target (a page key such as "users", or a path) into a route path; unknown targets fall back to the overview.
export function pageForTarget(target: string): string {
  if (target.startsWith("/")) return pageForPath(target)?.path ?? legacyRedirects[target] ?? "/";
  return navItems.find(item => item.key === target)?.path ?? "/";
}

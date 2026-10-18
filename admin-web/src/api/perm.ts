import { z } from "zod";
import { candidateCategoryLabel } from "./community";
import type { Permission } from "./shell";
import { roleLabel } from "./shell";

// Matrix row labels, in AllAdminPermissions order.
export const permissionLabels: Record<Permission, string> = {
  review_dict_pr: "维护词库与审核 PR",
  review_community: "审核社区内容",
  triage_issues: "Issue 分诊",
  ban_users: "封禁账号",
  publish_notices: "发布公告",
  trigger_release: "触发发布流水线",
  view_cloud_usage: "查看云服务用量",
  view_logs: "查看服务日志",
  manage_permissions: "管理权限",
};

export function permissionLabel(key: string): string {
  return permissionLabels[key as Permission] ?? key;
}

export const roleSchema = z.object({ key: z.string(), name: z.string(), builtin: z.boolean() });
export type Role = z.infer<typeof roleSchema>;

export const memberSchema = z.object({
  email: z.string(),
  role: z.string(),
  enabled: z.boolean(),
  owner: z.boolean(),
  sessions: z.number().int().nonnegative(),
  last_seen_at: z.string().nullable(),
  created_at: z.string().nullable(),
});
export type Member = z.infer<typeof memberSchema>;

// GET /api/permissions: roles, the role → permissions matrix and every admin (configured owners first).
export const permissionsSchema = z.object({
  roles: z.array(roleSchema),
  permissions: z.array(z.string()),
  matrix: z.record(z.string(), z.array(z.string())),
  members: z.array(memberSchema),
});
export type Permissions = z.infer<typeof permissionsSchema>;

// POST /api/permissions {action: grant|revoke, role, permission} and POST /api/admins {action, email, role?}.
export const changeResultSchema = z.object({ ok: z.literal(true), affected: z.number() });

export const auditEntrySchema = z.object({
  id: z.number(),
  actor: z.string().optional(),
  action: z.string(),
  target: z.string(),
  detail: z.record(z.string(), z.unknown()).nullish().transform(value => value ?? {}),
  created_at: z.string(),
});
export type AuditEntry = z.infer<typeof auditEntrySchema>;

// GET /api/audit?page=: 50 entries per page, newest first.
export const auditListSchema = z.object({ items: z.array(auditEntrySchema), page: z.number(), total: z.number(), has_more: z.boolean() });

// actorName shows an audit actor the way the console names people: the local part of the email.
export function actorName(actor: string | undefined): string {
  if (!actor || actor === "legacy-token") return "旧版令牌";
  const email = actor.startsWith("pat:") ? actor.slice(4) : actor.startsWith("google:") ? actor.slice(actor.lastIndexOf(":") + 1) : actor;
  return email.split("@")[0] || email;
}

export function initial(name: string): string {
  return (Array.from(name.trim())[0] ?? "?").toUpperCase();
}

const prefLabels: Record<string, string> = {
  notify_dict_pr: "词库 PR 提醒",
  notify_report: "社区举报提醒",
  notify_crash_spike: "崩溃告警",
  weekly_digest: "每周摘要邮件",
};

const sectionLabels: Record<string, string> = { skins: "键盘皮肤", "candidate-skins": "候选窗皮肤", plugins: "插件", dictionaries: "社区词库", replies: "回复模板" };
const crashStates: Record<string, string> = { open: "未处理", known: "已知问题", fixed: "已修复" };

function text(value: unknown): string {
  return typeof value === "string" || typeof value === "number" ? String(value) : "";
}

// describeAudit phrases an audit entry from its action and detail, for example 「通过了词库 PR #210（5 条）」. Unknown actions fall back to the raw action name so nothing is hidden.
export function describeAudit(entry: AuditEntry): string {
  const d = entry.detail;
  const target = entry.target;
  const who = target.includes("@") ? target.split("@")[0] : target;
  const reason = text(d.reason);
  const suffix = reason ? `（${reason}）` : "";
  const count = text(d.count);
  const section = sectionLabels[text(d.section)] ?? "社区内容";
  const item = target ? `「${target}」` : count ? ` ${count} 项` : "";
  switch (entry.action) {
    case "create_skin": case "create_candidate_skin": case "create_plugin": return `新增了${section}「${text(d.name) || target}」`;
    case "update_skin": case "update_candidate_skin": case "update_plugin": return `编辑了${section}「${text(d.name) || target}」`;
    case "dictionary_change": return `为 ${target} 提交了词条变更 PR #${text(d.pull)}（${count} 项）`;
    case "dict_pr_approve": return `通过了词库 PR #${target}${count ? `（${count} 条）` : ""}`;
    case "dict_pr_reject": return `驳回了词库 PR #${target}${suffix}`;
    case "dict_pr_trim": return `精简了词库 PR #${target}${count ? `（保留 ${count} 条）` : ""}`;
    case "approve_content": return `通过了${section}${item}`;
    case "remove_content": return `下架了${section}${item}${suffix}`;
    case "restore_content": return `恢复了${section}${item}`;
    case "set_skin_category": return `将键盘皮肤${text(d.name) ? `「${text(d.name)}」` : count ? ` ${count} 项` : ""}的分类改为「${candidateCategoryLabel(text(d.category))}」`;
    case "set_candidate_skin_category": return `将候选窗皮肤${text(d.name) ? `「${text(d.name)}」` : count ? ` ${count} 项` : ""}的分类改为「${candidateCategoryLabel(text(d.category))}」`;
    case "delete_skin": return `删除了键盘皮肤「${target}」${suffix}`;
    case "delete_candidate_skin": return `删除了候选窗皮肤「${target}」${suffix}`;
    case "delete_plugin": return `删除了插件「${target}」${suffix}`;
    case "delete_dictionary": return `删除了社区词库「${target}」${suffix}`;
    case "delete_reply": return `删除了回复「${target}」${suffix}`;
    case "ban_user": return `封禁了账号 ${target}${suffix}`;
    case "unban_user": return `解封了账号 ${target}`;
    case "revoke_session": case "revoke_sessions": return `撤销了账号 ${target} 的登录会话`;
    case "add_sensitive_word": return `添加了敏感词${text(d.pattern) ? `「${text(d.pattern)}」` : ""}`;
    case "set_sensitive_word_level": return "调整了敏感词处理方式";
    case "delete_sensitive_word": return "删除了敏感词";
    case "save_notice_draft": return `保存了公告草稿${text(d.title) ? `「${text(d.title)}」` : ""}`;
    case "publish_notice": return `发布公告${text(d.title) ? `「${text(d.title)}」` : ""}`;
    case "archive_notice": return `归档了公告${text(d.title) ? `「${text(d.title)}」` : ""}`;
    case "resolve_crash": return "标记崩溃已解决";
    case "reopen_crash": return "重新打开了崩溃记录";
    case "crash_group_issue_attempt": return "尝试为崩溃分组创建 Issue，并标记为已知问题";
    case "crash_group_issue_rejected": return "GitHub 拒绝创建崩溃 Issue，恢复为未处理";
    case "crash_group_status": return `将崩溃分组标记为「${crashStates[text(d.to)] ?? text(d.to)}」`;
    case "open_incident": return `开启了事件${text(d.title) ? `「${text(d.title)}」` : ""}`;
    case "resolve_incident": return "解决了事件";
    case "update_incident": return "更新了事件说明";
    case "release_trigger": return d.uncertain === true
      ? `尝试触发 ${[text(d.platform), text(d.version)].filter(Boolean).join(" ") || target} 发布，结果未确认`
      : `触发了 ${[text(d.platform), text(d.version)].filter(Boolean).join(" ") || target} 发布`;
    case "admin_add": return `添加管理员 ${who}${text(d.role) ? `（${roleLabel(text(d.role))}）` : ""}`;
    case "admin_enable": return `重新启用了管理员 ${who}`;
    case "admin_disable": return `停用了管理员 ${who}`;
    case "admin_revoke": return `撤销了管理员 ${who} 的全部会话`;
    case "admin_set_role": return `授予 ${who}「${roleLabel(text(d.to))}」角色`;
    case "permission_grant": return `授予「${text(d.role_name) || roleLabel(text(d.role))}」：${permissionLabel(text(d.permission))}`;
    case "permission_revoke": return `收回「${text(d.role_name) || roleLabel(text(d.role))}」：${permissionLabel(text(d.permission))}`;
    case "admin_pref_set": return `${d.value === true ? "开启" : "关闭"}了「${prefLabels[text(d.key)] ?? text(d.key)}」`;
    case "admin_session_revoke": return "退出了一个登录设备";
    case "admin_token_regenerate": return "重新生成了个人访问令牌";
    default:
      if (entry.action.startsWith("issue_")) return `处理了 Issue ${target}（${entry.action.slice(6)}）`;
      if (entry.action.startsWith("release_")) return `发布操作 ${entry.action.slice(8)}：${target}`;
      return `${entry.action}${target ? ` ${target}` : ""}${suffix}`;
  }
}

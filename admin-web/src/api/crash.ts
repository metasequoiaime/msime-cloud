import { z } from "zod";
import { actionUndoTokenSchema } from "./action-undo.ts";

export const crashStatusSchema = z.enum(["open", "known", "fixed"]);
export type CrashStatus = z.infer<typeof crashStatusSchema>;

export const crashStatusUndoSchema = z.object({
  action: z.literal("crash_group_status"), id: z.string(),
  value: z.object({ status: crashStatusSchema, token: actionUndoTokenSchema }),
});

// GET /api/crash-groups row. devices_7d is null while clients do not report install ids.
export const crashGroupSchema = z.object({
  signature: z.string(),
  platform: z.string(),
  version: z.string(),
  title: z.string(),
  status: crashStatusSchema,
  issue_url: z.string().nullable(),
  first_seen: z.string(),
  last_seen: z.string(),
  count_7d: z.number(),
  count_prev_7d: z.number(),
  devices_7d: z.number().nullable(),
  new: z.boolean(),
});
export type CrashGroup = z.infer<typeof crashGroupSchema>;

// 近 7 天某个版本的会话统计；insufficient 表示会话数据不足，此时 crash_free_rate 为 null 且不计入合计。
export const sessionVersionSchema = z.object({
  platform: z.string(),
  version: z.string(),
  sessions: z.number(),
  crashed_sessions: z.number(),
  active_devices: z.number(),
  crash_free_rate: z.number().nullable(),
  insufficient: z.boolean(),
});
export type SessionVersion = z.infer<typeof sessionVersionSchema>;

// 顶部统计。installs_today、crash_free_rate、active_devices_7d 依赖可选的 active/session 遥测，客户端未上报时为 null；crash_free_rate 只合计会话数据充分的版本。devices_7d 统计全部崩溃事件，其中真正崩溃的设备是 crash_devices_7d、被系统强制终止的设备是 forced_termination_devices_7d；device_crash_rate 是 crash_devices_7d 除以 active_devices_7d。
export const crashSummarySchema = z.object({
  groups: z.number(),
  crashes_7d: z.number(),
  devices_7d: z.number().nullable(),
  installs_today: z.number().nullable(),
  crash_free_rate: z.number().nullable(),
  active_devices_7d: z.number().nullable(),
  device_crash_rate: z.number().nullable(),
  versions: z.array(sessionVersionSchema),
  crash_devices_7d: z.number(),
  forced_termination_devices_7d: z.number(),
});

export const crashGroupsSchema = z.object({
  items: z.array(crashGroupSchema),
  has_more: z.boolean(),
  platforms: z.array(z.object({ platform: z.string(), count: z.number() })),
  summary: crashSummarySchema,
});

export const crashSampleSchema = z.object({
  id: z.string(),
  platform: z.string(),
  version: z.string(),
  message: z.string(),
  stack: z.string(),
  resolved: z.boolean(),
  created_at: z.string(),
});

// GET /api/crash-groups/{signature}: the group and its latest 5 crashes.
export const crashGroupDetailSchema = z.object({ group: crashGroupSchema, samples: z.array(crashSampleSchema) });

// GET /api/crash-groups/{signature}/issue: where an issue would be opened; target is null when the platform has no repository in admin.github.platforms.
export const crashIssueTargetSchema = z.object({
  target: z.object({ platform: z.string(), name: z.string(), repo: z.string() }).nullable(),
  issue_url: z.string().nullable(),
});
export type CrashIssueTarget = z.infer<typeof crashIssueTargetSchema>;

// POST /api/crash-groups/{signature}/issue.
export const crashIssueCreatedSchema = z.object({ ok: z.literal(true), status: crashStatusSchema, issue_url: z.string(), number: z.number(), repo: z.string() });

export const crashStatusLabel: Record<CrashStatus, string> = { open: "未处理", known: "已知问题", fixed: "已修复" };
export const crashStatusTone = { open: "bad", known: "warn", fixed: "ok" } as const satisfies Record<CrashStatus, "bad" | "warn" | "ok">;

// Telemetry reports lowercase platform ids; the console shows the product names.
const platformOrder = ["windows", "macos", "linux", "android", "ios", "harmonyos", "harmony", "ohos"];

export { platformName } from "../utils/platform.ts";

export function comparePlatforms(a: string, b: string): number {
  const rank = (p: string) => {
    const index = platformOrder.indexOf(p.toLowerCase());
    return index < 0 ? platformOrder.length : index;
  };
  return rank(a) - rank(b) || a.localeCompare(b);
}

// crashTrend phrases week-over-week change like the design: 「新」, 「↑ 32%」, 「↓ 8%」 or 「持平」; rising marks it in the bad color.
export function crashTrend(group: Pick<CrashGroup, "count_7d" | "count_prev_7d" | "new">): { text: string; rising: boolean } {
  if (group.new) return { text: "新", rising: true };
  const { count_7d: now, count_prev_7d: before } = group;
  if (before === 0) return now > 0 ? { text: "↑", rising: true } : { text: "持平", rising: false };
  const change = Math.round(((now - before) / before) * 100);
  if (change === 0) return { text: "持平", rising: false };
  return change > 0 ? { text: `↑ ${change}%`, rising: true } : { text: `↓ ${-change}%`, rising: false };
}

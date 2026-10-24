import { z } from "zod";

const count = z.number().int().nonnegative();

export const serviceStateSchema = z.enum(["ok", "degraded", "down", "unknown"]);
export type ServiceState = z.infer<typeof serviceStateSchema>;

export const overviewServiceSchema = z.object({
  key: z.string(),
  name: z.string(),
  provider: z.string(),
  state: serviceStateSchema,
  // uptime_60d is a percentage (0–100); null until the status probe has recorded a day.
  uptime_60d: z.number().nullable(),
  p95_ms: z.number().int().nullable(),
});
export type OverviewService = z.infer<typeof overviewServiceSchema>;

export const activeDaySchema = z.object({ day: z.string(), windows: count, macos: count, linux: count, android: count, ios: count, harmonyos: count });
export type ActiveDay = z.infer<typeof activeDaySchema>;

export const emailDeliveryDaySchema = z.object({ day: z.string(), attempted: count, accepted: count, rejected: count, failed: count });
export type EmailDeliveryDay = z.infer<typeof emailDeliveryDaySchema>;

// 各平台近 7 天的稳定性：crash_free_rate 只合计会话数据充分的版本，versions 是有会话的版本数，insufficient_versions 是其中因会话数据不足未计入的版本数；device_crash_rate 是真正崩溃的设备除以活跃设备，被系统强制终止的设备单独计入 forced_termination_devices_7d 和 forced_termination_rate。
export const crashPlatformSchema = z.object({
  platform: z.string(),
  crash_free_rate: z.number().nullable(),
  sessions: count,
  crashed_sessions: count,
  versions: count,
  insufficient_versions: count,
  active_devices_7d: count,
  crash_devices_7d: count,
  device_crash_rate: z.number().nullable(),
  forced_termination_devices_7d: count,
  forced_termination_rate: z.number().nullable(),
});
export type CrashPlatform = z.infer<typeof crashPlatformSchema>;

// GET /api/overview?days=7|30. Totals, the daily series, the telemetry-backed device metrics (zero or null until clients report active/session events; telemetry says whether they do), database-side pending work and the monitored services.
export const overviewSchema = z.object({
  users: count,
  new_users_30d: count,
  downloads: count,
  downloads_30d: count,
  downloads_prev_30d: count,
  crashes: count,
  open_crashes: count,
  range_days: z.number().int(),
  email_delivery: z.array(emailDeliveryDaySchema).nullable().transform(rows => rows ?? []),
  telemetry: z.object({ active: z.boolean(), sessions: z.boolean() }),
  active_devices_daily: z.array(activeDaySchema).nullable().transform(rows => rows ?? []),
  active_devices_7d: count,
  active_devices_prev_7d: count,
  platform_active_7d: z.record(z.string(), count),
  // crash_free_rate 是近 7 天的无崩溃会话率（0–1），只合计会话数据充分的版本，没有这样的会话时为 null；crash_free_rate_prev 是再往前 7 天的同口径比率。
  crash_free_rate: z.number().nullable(),
  crash_free_rate_prev: z.number().nullable(),
  // crash_top 是会话数据充分的版本中近 7 天崩溃会话最多的平台和版本；crash_group_latest 是该窗口内首次出现的最新未处理崩溃分组。没有数据时都为 null。
  crash_top: z.object({ platform: z.string(), version: z.string(), crashes: count }).nullable(),
  // crash_devices_7d 是近 7 天有崩溃的去重设备数，device_crash_rate 是它除以近 7 天活跃设备的比率，没有活跃设备时为 null。
  crash_devices_7d: count,
  device_crash_rate: z.number().nullable(),
  forced_termination_devices_7d: count,
  crash_platforms: z.array(crashPlatformSchema),
  crash_group_latest: z.object({ signature: z.string(), platform: z.string(), title: z.string() }).nullable(),
  pending: z.object({ community: count, reports_7d: count, crash_groups: count }),
  services_configured: z.boolean(),
  services: z.array(overviewServiceSchema),
});
export type Overview = z.infer<typeof overviewSchema>;

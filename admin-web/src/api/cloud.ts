import { z } from "zod";

// Service states shared by /api/cloud and /api/status: idle means no calls in the last five minutes (counted as available); unknown means no probe has run yet.
export const serviceStateSchema = z.enum(["ok", "idle", "degraded", "down", "unknown"]);
export type ServiceState = z.infer<typeof serviceStateSchema>;

const cloudHourSchema = z.object({ hour: z.string(), calls: z.number(), errors: z.number() });

// GET /api/cloud (requires view_cloud_usage): per configured upstream service, the last 24 hours and this UTC month's usage against the admin.services quota.
export const cloudServiceSchema = z.object({
  key: z.string(),
  name: z.string(),
  provider: z.string(),
  state: serviceStateSchema,
  calls_24h: z.number(),
  errors_24h: z.number(),
  error_rate: z.number().nullable(),
  p95_ms: z.number().nullable(),
  slow_ms: z.number(),
  hourly: z.array(cloudHourSchema),
  // meter is how the service measures usage: calls, characters (translation) or seconds (speech).
  month: z.object({ calls: z.number(), errors: z.number(), usage: z.number(), meter: z.enum(["calls", "chars", "seconds"]) }),
  quota: z.object({ limit: z.number(), unit: z.enum(["calls", "chars", "hours", "cny"]), used: z.number(), pct: z.number() }).nullable(),
  cost_cny: z.number().nullable(),
});
export type CloudService = z.infer<typeof cloudServiceSchema>;

export const cloudSchema = z.object({
  generated_at: z.string(),
  since: z.string(),
  month_start: z.string(),
  services: z.array(cloudServiceSchema),
});

// formatLatency writes a latency the way the design does: 310ms, 1.8s, or — without data.
export function formatLatency(ms: number | null | undefined): string {
  if (ms == null || ms <= 0) return "—";
  return ms >= 1000 ? `${(ms / 1000).toFixed(ms >= 10000 ? 0 : 1)}s` : `${Math.round(ms)}ms`;
}

// 保留云服务与系统状态页的既有导出，格式化逻辑由公共数字工具提供。
export { formatRate } from "../utils/number";

// stateTone maps a service state to the dot and pill tone: slow or failing is warn, down is bad, no probe yet is muted.
export function stateTone(state: ServiceState): "ok" | "warn" | "bad" | "mute" {
  if (state === "degraded") return "warn";
  if (state === "down") return "bad";
  if (state === "unknown") return "mute";
  return "ok";
}

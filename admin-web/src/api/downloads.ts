import { z } from "zod";
import { platformName } from "../utils/platform.ts";

const count = z.number().int().nonnegative();

// One group of GET /api/downloads/summary: telemetry rows group reported download events, github_release rows are GitHub Release asset downloads taken from daily snapshots.
export const downloadRowSchema = z.object({
  source: z.enum(["telemetry", "github_release"]),
  platform: z.string(),
  version: z.string(),
  artifact: z.string().nullable(),
  channel: z.string().nullable(),
  repo: z.string().optional(),
  tag: z.string().optional(),
  today: count,
  week: count,
});
export type DownloadRow = z.infer<typeof downloadRowSchema>;

// GET /api/downloads/summary. Days are UTC; mirror_share is null while no download event of the week reports a channel.
export const downloadsSummarySchema = z.object({
  day: z.string(),
  rows: z.array(downloadRowSchema),
  totals: z.object({ today: count, week: count, github_today: count, github_week: count, mirror_week: count }),
  mirror_share: z.number().min(0).max(1).nullable(),
  channel_reported: z.boolean(),
  snapshot_day: z.string().nullable(),
  truncated: z.boolean(),
});
export type DownloadsSummary = z.infer<typeof downloadsSummarySchema>;

// 下载筛选沿用公共平台选项和规范化键，保留原有导出供页面使用。
export { platformKey, platformOptions } from "../utils/platform.ts";

export function platformLabel(platform: string): string {
  return platformName(platform) || "未知平台";
}

const channelLabels: Record<string, string> = {
  "cn-mirror": "官网镜像（国内）",
  website: "官网",
  github: "GitHub（客户端上报）",
  "app-store": "App Store",
  testflight: "TestFlight",
  appgallery: "AppGallery",
  "google-play": "Google Play",
};

// channelLabel 按来源显示渠道；遥测的未知键保留原文，不能把原型属性当作已知标签。
export function channelLabel(row: Pick<DownloadRow, "source" | "channel">): string {
  if (row.source === "github_release") return "GitHub Release";
  if (!row.channel) return "未上报渠道";
  return Object.hasOwn(channelLabels, row.channel) ? channelLabels[row.channel] : row.channel;
}

// GET/POST /api/site-settings: the Lanzou cloud link the official website's download page offers for the Windows installer. updated_at and updated_by stay set after the link is cleared.
export const siteSettingsSchema = z.object({ lanzou_url: z.string(), updated_at: z.string(), updated_by: z.string() });
export type SiteSettings = z.infer<typeof siteSettingsSchema>;

// lanzouURLError mirrors the server rule (empty, or an absolute https URL with a host, no credentials, at most 512 bytes) so obvious mistakes are caught before a request; the server stays authoritative.
export function lanzouURLError(value: string): string | null {
  if (value === "") return null;
  if (new TextEncoder().encode(value).length > 512) return "链接不能超过 512 字节。";
  if (!value.startsWith("https://") || /[\s\p{Cc}]/u.test(value)) return "请输入以 https:// 开头的完整链接。";
  try {
    const url = new URL(value);
    if (url.protocol !== "https:" || url.hostname === "") return "请输入以 https:// 开头的完整链接。";
    if (url.username !== "" || url.password !== "") return "链接不能包含账号密码。";
  } catch {
    return "请输入以 https:// 开头的完整链接。";
  }
  return null;
}

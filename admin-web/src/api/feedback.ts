import { z } from "zod";

// GET /api/feedback：App 内反馈的一页（50 条）。diagnostics 只含服务端白名单里的键，截图字节经 GET /api/feedback/{id}/screenshots/{n} 单独读取。
export const feedbackTypes = ["bug", "suggestion", "dictionary"] as const;
export type FeedbackType = (typeof feedbackTypes)[number];
export const feedbackTypeLabels: Record<FeedbackType, string> = { bug: "问题", suggestion: "建议", dictionary: "词库" };

export const feedbackStatuses = ["new", "resolved"] as const;
export type FeedbackStatus = (typeof feedbackStatuses)[number];
export const feedbackStatusLabels: Record<FeedbackStatus, string> = { new: "待处理", resolved: "已处理" };
export const feedbackStatusTones = { new: "warn", resolved: "ok" } as const satisfies Record<FeedbackStatus, "warn" | "ok">;

export const feedbackPlatforms = ["android", "ios", "macos", "windows", "linux", "harmony"] as const;

export const feedbackRowSchema = z.object({
  id: z.string(),
  type: z.enum(feedbackTypes),
  text: z.string(),
  platform: z.string(),
  app_version: z.string(),
  edition: z.string(),
  diagnostics: z.record(z.string(), z.string()).nullish(),
  status: z.enum(feedbackStatuses),
  created_at: z.string(),
  user_id: z.string(),
  author: z.string(),
  anonymous: z.boolean(),
  screenshots: z.number().int().nonnegative(),
});
export type FeedbackRow = z.infer<typeof feedbackRowSchema>;
export const feedbackListSchema = z.object({ items: z.array(feedbackRowSchema), page: z.number(), total: z.number(), has_more: z.boolean() });

// 诊断信息键的中文名；没有列出的键按原样显示。
export const diagnosticLabels: Record<string, string> = {
  device: "设备", os: "系统", app_version: "App 版本", edition: "版本", scheme: "输入方案", keyboard_layout: "键盘布局", skin: "皮肤", ime_enabled: "已启用输入法", ime_default: "默认输入法",
};

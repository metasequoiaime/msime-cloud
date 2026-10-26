import { z } from "zod";
import { paginatedSchema } from "./paginated.ts";
import { actionUndoTokenSchema } from "./action-undo.ts";

// 社区管理：六类内容分区、各自的列表、计数和抽屉详情。

export const sections = ["skins", "candidate-skins", "plugins", "dictionaries", "replies", "phrases"] as const;
export type Section = (typeof sections)[number];
export const sectionLabels: Record<Section, string> = { skins: "键盘皮肤", "candidate-skins": "候选窗皮肤", plugins: "插件", dictionaries: "词库", replies: "回复模板", phrases: "短语包" };

export function isSection(value: string | undefined): value is Section {
  return (sections as readonly string[]).includes(value ?? "");
}

export const moderationStates = ["pending", "approved", "removed"] as const;
export type Moderation = (typeof moderationStates)[number];
export const moderationSchema = z.enum(moderationStates);
export const moderationLabels: Record<Moderation, string> = { pending: "待审核", approved: "已上架", removed: "已下架" };
export const moderationTones = { pending: "warn", approved: "ok", removed: "bad" } as const;

const stateCountsSchema = z.object({ pending: z.number().int().nonnegative(), approved: z.number().int().nonnegative(), removed: z.number().int().nonnegative() });
export const countsSchema = z.object({
  skins: stateCountsSchema, "candidate-skins": stateCountsSchema, plugins: stateCountsSchema, dictionaries: stateCountsSchema, replies: stateCountsSchema, phrases: stateCountsSchema,
});
export type Counts = z.infer<typeof countsSchema>;

// 键盘皮肤与候选窗皮肤共用的图库分类，取值与服务端 candidateSkinCategories 一致。
export const candidateCategories = ["nature", "guofeng", "acg", "cute", "food", "tech", "minimal", "other"] as const;
export type CandidateCategory = (typeof candidateCategories)[number];
export const categoryUndoSchema = z.object({
  action: z.enum(["set_skin_category", "set_candidate_skin_category"]),
  section: z.enum(["skins", "candidate-skins"]), id: z.string(),
  value: z.object({ category: z.enum(candidateCategories), token: actionUndoTokenSchema }),
});
export const moderationUndoSchema = z.object({
  action: z.literal("undo_moderation"), section: z.enum(sections), id: z.string(),
  value: z.object({ token: actionUndoTokenSchema, moderation: moderationSchema, previous: z.enum(["pending", "approved"]).nullable(), reason: z.string().nullable() }),
});
export const candidateCategoryLabels: Record<CandidateCategory, string> = { nature: "自然", guofeng: "国风", acg: "二次元", cute: "可爱", food: "美食", tech: "科技夜色", minimal: "简约", other: "其他" };

export function candidateCategoryLabel(value: string | null | undefined): string {
  return candidateCategoryLabels[value as CandidateCategory] ?? value ?? "—";
}

export function isCandidateCategory(value: string): value is CandidateCategory {
  return (candidateCategories as readonly string[]).includes(value);
}

const entrySchema = z.object({ kind: z.string(), code: z.string(), word: z.string(), weight: z.number() });
export type Entry = z.infer<typeof entrySchema>;

// itemSchema is one list row of any section; the fields after reports exist only for some sections.
export const itemSchema = z.object({
  edit_token: z.string().optional(),
  id: z.string(),
  name: z.string(),
  description: z.string().nullish(),
  owner_id: z.string(),
  author: z.string(),
  created_at: z.string(),
  updated_at: z.string().nullish(),
  moderation: moderationSchema,
  previous_moderation: moderationSchema.nullish(),
  moderation_reason: z.string().nullish(),
  moderated_by: z.string().nullish(),
  moderated_at: z.string().nullish(),
  flag: z.string().nullish(),
  reports: z.number().int().nonnegative(),
  design: z.unknown().optional(),
  downloads: z.number().optional(),
  saves: z.number().optional(),
  package_id: z.string().optional(),
  version: z.string().optional(),
  size: z.number().optional(),
  file_count: z.number().optional(),
  visibility: z.enum(["public", "private"]).optional(),
  category: z.string().optional(),
  kind: z.string().optional(),
  plugin_id: z.string().optional(),
  // 脚本插件申请的能力、其中要重点看的能力，以及它是否先审后发（待审期间不公开）。
  permissions: z.array(z.string()).optional(),
  flagged_permissions: z.array(z.string()).optional(),
  pre_moderated: z.boolean().optional(),
  entries: z.number().nullish(),
  preview: z.array(entrySchema).nullish(),
  prompt: z.string().nullish(),
  // 短语包卡片的前三条正文。
  phrases: z.array(z.string()).nullish(),
});
export type Item = z.infer<typeof itemSchema>;

export const listSchema = paginatedSchema(itemSchema);

// 卡片预览只读取设计或清单，完整审核信息在打开详情时加载。
export const contentPreviewSchema = z.object({ content: z.unknown() });

const reportSchema = z.object({ id: z.number(), reason: z.string(), detail: z.string(), reporter: z.string(), created_at: z.string() });
const flagSchema = z.object({ word_id: z.number(), pattern: z.string(), category: z.string(), level: z.string() });
const ownerItemSchema = z.object({ section: z.enum(sections), id: z.string(), name: z.string(), moderation: moderationSchema, created_at: z.string() });

// detailSchema is GET /api/<section>/<id>: the stored item with its moderation state, user reports, live sensitive-word hits and the author's other works.
export const detailSchema = z.object({
	edit_token: z.string().optional(),
  id: z.string(),
  name: z.string(),
  description: z.string().nullish(),
  owner_id: z.string(),
  author: z.string().nullish(),
  created_at: z.string(),
  updated_at: z.string().nullish(),
  content: z.unknown(),
  moderation: moderationSchema,
  previous_moderation: moderationSchema.nullish(),
  moderation_reason: z.string().nullish(),
  moderated_by: z.string().nullish(),
  moderated_at: z.string().nullish(),
  owner_banned: z.boolean(),
  downloads: z.number().optional(),
  saves: z.number().optional(),
  rating_count: z.number().optional(),
  rating_average: z.number().optional(),
  revision: z.number().optional(),
  version: z.string().optional(),
  visibility: z.enum(["public", "private"]).optional(),
  category: z.string().optional(),
  package_id: z.string().optional(),
  plugin_id: z.string().optional(),
  kind: z.string().optional(),
  permissions: z.array(z.string()).optional(),
  pre_moderated: z.boolean().optional(),
  size: z.number().optional(),
  sha256: z.string().optional(),
  license: z.union([z.string(), z.object({ code: z.string().nullish(), assets: z.string().nullish(), source: z.string().nullish() })]).optional(),
  files: z.array(z.object({ path: z.string(), size: z.number(), sha256: z.string() })).optional(),
  reports: z.array(reportSchema),
  report_count: z.number().int().nonnegative(),
  flags: z.array(flagSchema),
  owner_items: z.array(ownerItemSchema),
});
export type Detail = z.infer<typeof detailSchema>;

const phraseSchema = z.object({ text: z.string(), group: z.string() });
export const resourceContentSchema = z.object({ entries: z.array(entrySchema).optional(), prompt: z.string().optional(), phrases: z.array(phraseSchema).optional() });

export const sensitiveLevelLabels: Record<string, string> = { block: "拦截", review: "需复核" };
export const sensitiveCategoryLabels: Record<string, string> = { ad: "广告导流", vulgar: "低俗", abuse: "辱骂", illegal: "违法", custom: "自定义" };
// 插件类型的统一显示名，与客户端、官网和 msime-plugins 一致。
export const pluginKindLabels: Record<string, string> = { sound: "音效包", music: "音乐包", command_table: "指令表", effect: "特效包", helpcode: "辅助码表", symbol_set: "符号集", phrase_table: "短语表", wordbook: "单词本", script: "脚本插件" };
// 脚本插件能力的白话名，与客户端授权页的标题一致；不认识的能力名原样显示。
export const scriptCapabilityLabels: Record<string, string> = {
  read_composition: "读取正在输入的内容",
  read_commit: "读取输入的文字",
  read_candidates: "读取候选词",
  rewrite_commit: "修改输入的文字",
  rerank_candidates: "调整候选顺序",
  annotate_candidates: "给候选加注释",
  filter_candidates: "隐藏候选",
  provide_commands: "提供 / 指令",
  insert_candidates: "往候选里加词",
  persist_data: "长期保存数据",
};
// 审核时要重点看的能力：能改掉用户上屏的文字，或能把读到的文字长期留在本机。服务端在 flagged_permissions 里标出同样两项。
export const flaggedScriptCapabilities = ["rewrite_commit", "persist_data"];
export function scriptCapabilityText(names: readonly string[] | undefined): string {
  if (!names || names.length === 0) return "不申请任何能力";
  return names.map(name => (scriptCapabilityLabels[name] ?? name) + (flaggedScriptCapabilities.includes(name) ? "（重点审查）" : "")).join("、");
}

// Reason presets of the 驳回 / 下架 confirm dialog.
export const removeReasons = ["侵犯版权或商标", "含导流或广告", "内容低俗", "质量不达标"] as const;

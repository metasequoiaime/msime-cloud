import { z } from "zod";

export const dictionaryFileSchema = z.object({ path: z.string(), type: z.literal("blob"), size: z.number().nonnegative(), sha: z.string(), url: z.string().optional() });
export type DictionaryFile = z.infer<typeof dictionaryFileSchema>;
export const dictionarySchema = z.object({
  repo: z.string(), branch: z.string(), url: z.string(), description: z.string().nullable(), pushed_at: z.string(),
  files: z.array(dictionaryFileSchema), truncated: z.boolean(),
  counts: z.object({ custom: z.number(), packs: z.number(), sources: z.number() }), bytes: z.number(),
});
export const dictionaryRowSchema = z.object({ line: z.number().int().positive(), columns: z.array(z.string()) });
export type DictionaryRow = z.infer<typeof dictionaryRowSchema>;
export const dictionaryContentSchema = z.object({
  repo: z.string(), name: z.string(), path: z.string(), sha: z.string(), size: z.number(), url: z.string(),
  rows: z.array(dictionaryRowSchema), total: z.number().int().nonnegative(), page: z.number().int().positive(), has_more: z.boolean(), encoding: z.string(), editable: z.boolean(),
});
export type DictionaryContent = z.infer<typeof dictionaryContentSchema>;
export const dictionaryChangeSchema = z.object({ number: z.number().int().positive(), url: z.string(), branch: z.string() });
export type DictionaryEdit = { operation: "add" | "replace" | "delete"; line: number; columns: string[] };
export const sectionLabels = { custom: "自定义词库", packs: "专题词包", sources: "原始来源" } as const;
export type Section = keyof typeof sectionLabels;
export function fileSection(file: Pick<DictionaryFile, "path">): Section {
  const key = file.path.split("/", 1)[0];
  return key === "packs" || key === "sources" ? key : "custom";
}
export function columnLabels(path: string, count: number): string[] {
  let labels: string[];
  if (path.endsWith("translations.txt")) labels = ["原词", "译文"];
  else if (path.endsWith("english.txt")) labels = ["编码", "显示词", "权重"];
  else if (path.startsWith("custom/") || path.startsWith("packs/")) labels = ["词语", "全拼", "权重"];
  else if (/sources\/japanese\/dictionary\d+\.txt$/.test(path)) labels = ["读音", "左上下文 ID", "右上下文 ID", "代价", "词语"];
  else if (path.startsWith("sources/korean/")) labels = ["韩文", "汉字", "说明"];
  else if (path.startsWith("sources/pinyin/") || path.startsWith("sources/wubi/") || path.startsWith("sources/stroke/") || path.includes(".dict.yaml")) labels = ["词语", "读音 / 编码", "权重"];
  else if (path.endsWith("tsi.csv") || path.endsWith("word.csv")) labels = ["词语", "词频", "注音"];
  else if (path.endsWith("names.txt")) labels = ["词语"];
  else labels = [];
  return Array.from({ length: Math.max(count, labels.length, 1) }, (_, index) => labels[index] ?? `字段 ${index + 1}`);
}

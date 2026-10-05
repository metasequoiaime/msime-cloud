import { format } from "date-fns";
import { zhCN } from "date-fns/locale";
import { z } from "zod";
import type { Item, Section } from "../../api/community";
import { candidateCategoryLabel, pluginKindLabels } from "../../api/community";

export function formatTime(value: string | null | undefined): string {
  if (!value) return "—";
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? value : format(date, "yyyy-MM-dd HH:mm", { locale: zhCN });
}

export function formatBytes(value: number): string {
  if (value < 1024) return `${value} B`;
  if (value < 1024 * 1024) return `${(value / 1024).toFixed(value < 10 * 1024 ? 1 : 0)} KB`;
  return `${(value / 1024 / 1024).toFixed(1)} MB`;
}

const count = (value: number) => value.toLocaleString("zh-CN");

// itemMeta is the card's stats line after the author, built only from real counters.
export function itemMeta(section: Section, item: Item): string {
  const parts: string[] = [];
  if ((section === "dictionaries" || section === "phrases") && item.entries != null) parts.push(`${count(item.entries)} 条`);
  if (item.downloads !== undefined) parts.push(`下载 ${count(item.downloads)}`);
  if (item.saves !== undefined) parts.push(`收藏 ${count(item.saves)}`);
  if ((section === "skins" || section === "candidate-skins") && item.category) parts.push(candidateCategoryLabel(item.category));
  if (section === "candidate-skins" && item.visibility === "private") parts.push("私有");
  if (item.reports > 0) parts.push(`被举报 ${item.reports} 次`);
  return parts.join(" · ");
}

// previewLines is the text shown in a non-skin card's preview area.
export function previewLines(section: Section, item: Item): string[] {
  switch (section) {
    case "dictionaries":
      return (item.preview ?? []).map(entry => `${entry.word}  ${entry.code}`);
    case "replies":
      return (item.prompt ?? "").split("\n").map(line => line.trim()).filter(Boolean).slice(0, 3);
    case "phrases":
      // 每条短语只取第一行，多行模板在抽屉里看全文。
      return (item.phrases ?? []).map(text => text.split("\n")[0].trim()).filter(Boolean);
    case "candidate-skins":
      return [item.package_id ?? "", `v${item.version ?? "?"} · ${item.file_count ?? 0} 个文件 · ${formatBytes(item.size ?? 0)}`, item.description ?? ""].filter(Boolean);
    case "plugins":
      return [pluginKindLabels[item.kind ?? ""] ?? item.kind ?? "", item.plugin_id ?? "", `v${item.version ?? "?"} · ${formatBytes(item.size ?? 0)}`].filter(Boolean);
    default:
      return [];
  }
}

const colorSchema = z.number().int().min(0).max(0xffffff);
const miniDesignSchema = z.object({ background: colorSchema, keyBackground: colorSchema, cornerRadius: z.number().min(0).max(20), gradientEnd: colorSchema.nullish(), gradientHorizontal: z.boolean().nullish() });

export type MiniSkin = { background: string; key: string; radius: number };

const hex = (value: number) => `#${value.toString(16).padStart(6, "0")}`;

// miniSkin derives the card keyboard's colors from a skin design, or null when the design is not the supported data format.
export function miniSkin(design: unknown): MiniSkin | null {
  const parsed = miniDesignSchema.safeParse(design);
  if (!parsed.success) return null;
  const d = parsed.data;
  const start = hex(d.background);
  const end = hex(d.gradientEnd ?? d.background);
  return { background: `linear-gradient(${d.gradientHorizontal ? "90deg" : "160deg"},${start},${end})`, key: hex(d.keyBackground), radius: Math.round(d.cornerRadius / 2) };
}

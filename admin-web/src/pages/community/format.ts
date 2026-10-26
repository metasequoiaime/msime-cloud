import { formatNumber } from "../../utils/number";
import { formatDateTime } from "../../utils/time";
import { splitBytes } from "../../utils/bytes";
import type { Item, Section } from "../../api/community";
import { candidateCategoryLabel, pluginKindLabels, scriptCapabilityText } from "../../api/community";
import { firstLine } from "../../utils/text";

export function formatTime(value: string | null | undefined): string {
  return value ? formatDateTime(value) : "—";
}

export function formatBytes(value: number): string {
  const { value: amount, unit } = splitBytes(value);
  if (unit === "B") return `${amount} B`;
  if (unit === "KB") return `${amount.toFixed(value < 10 * 1024 ? 1 : 0)} KB`;
  return `${amount.toFixed(1)} MB`;
}

// itemMeta is the card's stats line after the author, built only from real counters.
export function itemMeta(section: Section, item: Item): string {
  const parts: string[] = [];
  if ((section === "dictionaries" || section === "phrases") && item.entries != null) parts.push(`${formatNumber(item.entries)} 条`);
  if (item.downloads !== undefined) parts.push(`下载 ${formatNumber(item.downloads)}`);
  if (item.saves !== undefined) parts.push(`收藏 ${formatNumber(item.saves)}`);
  if ((section === "skins" || section === "candidate-skins") && item.category) parts.push(candidateCategoryLabel(item.category));
  if (section === "candidate-skins" && item.visibility === "private") parts.push("私有");
  // 脚本插件先审后发：待审期间别人看不到，需要尽快处理。
  if (section === "plugins" && item.pre_moderated && item.moderation === "pending") parts.push("待审期间不公开");
  if (section === "plugins" && item.flagged_permissions && item.flagged_permissions.length > 0) parts.push("含需重点审查的能力");
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
      return (item.phrases ?? []).map(firstLine).filter(Boolean);
    case "candidate-skins":
      return [item.package_id ?? "", `v${item.version ?? "?"} · ${item.file_count ?? 0} 个文件 · ${formatBytes(item.size ?? 0)}`, item.description ?? ""].filter(Boolean);
    case "plugins":
      return [pluginKindLabels[item.kind ?? ""] ?? item.kind ?? "", item.plugin_id ?? "", `v${item.version ?? "?"} · ${formatBytes(item.size ?? 0)}`, item.kind === "script" ? `能力：${scriptCapabilityText(item.permissions)}` : ""].filter(Boolean);
    default:
      return [];
  }
}

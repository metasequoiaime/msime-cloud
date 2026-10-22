import { formatDateTime } from "../../utils/time";
import { codeMessage } from "../../api/client";
import { splitBytes } from "../../utils/bytes";
import { lastPathSegment } from "../../utils/path";
import type { Release, ReleaseStatus } from "../../api/release";
import type { Tone } from "../../ui/pill";

export const statusLabel: Record<ReleaseStatus, string> = { released: "已发布", beta: "公开测试", draft: "待发布", withdrawn: "已撤回" };
export const statusTone: Record<ReleaseStatus, Tone> = { released: "ok", beta: "info", draft: "mute", withdrawn: "bad" };

// Note kinds follow the "### 新增 / 修复 / 改进 / 说明 / 待办" headings of the release body.
export const noteKindClass: Record<string, string> = {
  新增: "bg-accent-soft text-accent-ink",
  修复: "bg-info-soft text-info",
  改进: "bg-panel-2 text-body",
  说明: "bg-warn-soft text-warn",
  待办: "bg-warn-soft text-warn",
};

// platformErrorText phrases the error code a listing reports for one platform instead of failing the whole request.
export function platformErrorText(code: string): string {
  return codeMessage(code) ?? "读取失败，请稍后重试。";
}

export function releaseDate(release: Release): string {
  return release.published_at ? formatDateTime(release.published_at, "yyyy-MM-dd") : "未发布";
}

export function repoShortName(repo: string): string {
  return lastPathSegment(repo);
}

export function formatSize(bytes: number): string {
  // 原有发布页将非数值视为未达到任何单位阈值，保留 B 文案。
  if (Number.isNaN(bytes)) return `${bytes} B`;
  const { value, unit } = splitBytes(bytes);
  if (unit === "MB") return `${value.toFixed(1)} MB`;
  if (unit === "KB") return `${Math.round(value)} KB`;
  return `${value} B`;
}

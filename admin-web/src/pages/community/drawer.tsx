import { useContext } from "react";
import { useQuery } from "@tanstack/react-query";
import { APICredentialsContext, APIError, useAPI } from "../../api/client";
import { keys } from "../../api/keys";
import type { CandidateCategory, Detail, Section } from "../../api/community";
import { candidateCategories, candidateCategoryLabels, detailSchema, isCandidateCategory, moderationLabels, moderationTones, pluginKindLabels, resourceContentSchema, sectionLabels, sensitiveCategoryLabels, sensitiveLevelLabels } from "../../api/community";
import { relativeTime } from "../../shell/notifications";
import { usePermissions } from "../../shell/permissions";
import { DetailDrawer } from "../../ui/drawer";
import type { DrawerAction, DrawerField, DrawerSection } from "../../ui/drawer";
import { KeyboardPreview } from "../../ui/keyboard-preview";
import type { Tone } from "../../ui/pill";
import { ErrorState, Skeleton, SkeletonRows } from "../../ui/states";
import type { Target } from "./actions";
import { formatBytes, formatTime } from "./format";

// usePreviewImage loads a candidate skin's preview through the admin API and turns it into a data: URL, which img-src allows, so it works with both the session cookie and the in-memory legacy token.
function usePreviewImage(id: string, enabled: boolean) {
  const credentials = useContext(APICredentialsContext);
  const token = credentials?.token;
  return useQuery({
    queryKey: keys.page("community", "preview", id),
    enabled,
    staleTime: 5 * 60_000,
    queryFn: async ({ signal }) => {
      const response = await fetch(`/api/candidate-skins/${encodeURIComponent(id)}/preview`, { signal, credentials: "same-origin", headers: token ? { Authorization: `Bearer ${token}` } : {} });
      if (!response.ok) throw new APIError(response.status, "");
      const blob = await response.blob();
      return await new Promise<string>((resolve, reject) => {
        const reader = new FileReader();
        reader.onload = () => resolve(String(reader.result));
        reader.onerror = () => reject(reader.error);
        reader.readAsDataURL(blob);
      });
    },
  });
}

function CandidatePreview({ id, name }: { id: string; name: string }) {
  const image = usePreviewImage(id, true);
  if (image.isPending) return <Skeleton className="mb-5 h-[180px] rounded-[14px]" />;
  if (image.isError) return <p className="m-0 mb-5 rounded-[14px] bg-panel-2 px-4 py-6 text-center text-[13px] text-muted">预览图加载失败</p>;
  return <img src={image.data} alt={`${name}预览图`} className="mb-5 block max-h-[260px] w-full rounded-[14px] bg-panel-2 object-contain" />;
}

function dataField(detail: Detail): string {
  const parts: string[] = [];
  if (detail.downloads !== undefined) parts.push(`下载 ${detail.downloads.toLocaleString("zh-CN")}`);
  if (detail.saves !== undefined) parts.push(`收藏 ${detail.saves.toLocaleString("zh-CN")}`);
  if (detail.rating_count) parts.push(`${(detail.rating_average ?? 0).toFixed(1)} 分（${detail.rating_count} 人）`);
  return parts.join(" · ") || "—";
}

function licenseText(license: Detail["license"]): string {
  if (!license) return "—";
  if (typeof license === "string") return license;
  return [license.code, license.assets && `素材 ${license.assets}`].filter(Boolean).join(" · ") || "—";
}

function sectionFields(section: Section, detail: Detail): DrawerField[] {
  const hits = `${detail.flags.length} 条`;
  switch (section) {
    case "candidate-skins":
      return [
        { label: "包 ID", value: detail.package_id ?? "—", mono: true },
        { label: "可见性", value: detail.visibility === "private" ? "私有库" : "公开图库" },
        { label: "授权", value: licenseText(detail.license) },
        { label: "文件", value: `${detail.files?.length ?? 0} 个 · ${formatBytes((detail.files ?? []).reduce((sum, file) => sum + file.size, 0))}` },
        { label: "敏感词命中", value: hits },
      ];
    case "plugins":
      return [
        { label: "类型", value: pluginKindLabels[detail.kind ?? ""] ?? detail.kind ?? "—" },
        { label: "插件 ID", value: detail.plugin_id ?? "—", mono: true },
        { label: "授权", value: licenseText(detail.license) },
        { label: "大小", value: formatBytes(detail.size ?? 0) },
        { label: "SHA-256", value: detail.sha256 ?? "—", mono: true },
        { label: "敏感词命中", value: hits },
      ];
    case "dictionaries": {
      const content = resourceContentSchema.safeParse(detail.content);
      return [{ label: "词条数", value: String(content.success ? content.data.entries?.length ?? 0 : 0) }, { label: "敏感词命中", value: hits }];
    }
    case "replies":
      return [{ label: "敏感词命中", value: hits }];
    case "phrases": {
      const content = resourceContentSchema.safeParse(detail.content);
      return [{ label: "短语数", value: String(content.success ? content.data.phrases?.length ?? 0 : 0) }, { label: "敏感词命中", value: hits }];
    }
    default:
      return [];
  }
}

function contentSection(section: Section, detail: Detail): DrawerSection | null {
  if (section === "dictionaries" || section === "replies" || section === "phrases") {
    const content = resourceContentSchema.safeParse(detail.content);
    if (!content.success) return { title: "内容预览", items: [], empty: "内容格式无法解析" };
    if (section === "replies") return { title: "内容预览", items: content.data.prompt ? [{ text: content.data.prompt }] : [], empty: "没有内容" };
    if (section === "phrases") {
      const phrases = content.data.phrases ?? [];
      return { title: `内容预览（${phrases.length} 条）`, empty: "没有短语", content: phrases.length > 0 && <div className="max-h-[320px] space-y-2 overflow-y-auto rounded-[10px] bg-panel-2 px-3 py-2.5 text-[13.5px] leading-relaxed">
        {phrases.map(phrase => <div key={`${phrase.group}\u0000${phrase.text}`} className="flex gap-3">
          <span className="min-w-0 flex-1 whitespace-pre-wrap break-words text-ink">{phrase.text}</span>
          {phrase.group && <span className="shrink-0 text-[12.5px] text-muted">{phrase.group}</span>}
        </div>)}
      </div>, items: phrases.length ? undefined : [] };
    }
    const entries = content.data.entries ?? [];
    return { title: `内容预览（${entries.length} 条）`, empty: "没有词条", content: entries.length > 0 && <div className="max-h-[280px] overflow-y-auto rounded-[10px] bg-panel-2 px-3 py-2 text-[13.5px] leading-[1.9]">
      {entries.map(entry => <div key={`${entry.kind}\u0000${entry.code}\u0000${entry.word}`} className="flex gap-3"><span className="min-w-0 flex-1 truncate text-ink">{entry.word}</span><span className="font-mono text-[12.5px] text-muted">{entry.code}</span></div>)}
    </div>, items: entries.length ? undefined : [] };
  }
  if (section === "candidate-skins" || section === "plugins") {
    const manifest = typeof detail.content === "string" ? detail.content : "";
    return { title: "清单文件", items: manifest ? undefined : [], empty: "没有清单", content: manifest && <pre className="m-0 max-h-[240px] overflow-auto rounded-[10px] bg-panel-2 px-3 py-2.5 font-mono text-xs leading-relaxed whitespace-pre-wrap text-body">{manifest}</pre> };
  }
  return null;
}

// CategorySelect 是键盘皮肤和候选皮肤详情里的分类下拉框；没有 review_community 权限时只读。
function CategorySelect({ value, disabled, onChange }: { value: CandidateCategory; disabled: boolean; onChange: (next: CandidateCategory) => void }) {
  return <select value={value} disabled={disabled} aria-label="分类" onChange={event => { if (isCandidateCategory(event.target.value)) onChange(event.target.value); }}
    className="h-8 max-w-full rounded-[9px] bg-panel px-2 text-[13px] text-ink inset-ring inset-ring-hair-2 disabled:opacity-45">
    {candidateCategories.map(category => <option key={category} value={category}>{candidateCategoryLabels[category]}</option>)}
  </select>;
}

export type ContentDrawerProps = {
  target: { section: Section; id: string } | null;
  onClose: () => void;
  onApprove: (target: Target) => void;
  onRemove: (target: Target) => void;
  onRestore: (target: Target) => void;
  onCategory: (target: Target, from: CandidateCategory, to: CandidateCategory) => void;
};

// ContentDrawer is the right-hand detail of one community item, with its reports, the live sensitive-word check and the author's other works.
export function ContentDrawer({ target, onClose, onApprove, onRemove, onRestore, onCategory }: ContentDrawerProps) {
  const api = useAPI();
  const { can } = usePermissions();
  const section = target?.section ?? "skins";
  const query = useQuery({
    queryKey: keys.page("community", "detail", section, target?.id),
    enabled: Boolean(target),
    queryFn: ({ signal }) => api.get(`${section}/${encodeURIComponent(target?.id ?? "")}`, detailSchema, { signal }),
  });
  const detail = query.data;
  const canReview = can("review_community");
  const pills: { text: string; tone: Tone }[] = [];
  const fields: DrawerField[] = [];
  const sections: DrawerSection[] = [];
  const actions: DrawerAction[] = [];
  if (detail) {
    const item: Target = { section, id: detail.id, name: detail.name, moderation: detail.moderation, moderation_reason: detail.moderation_reason, previous_moderation: detail.previous_moderation, created_at: detail.created_at, updated_at: detail.updated_at };
    pills.push({ text: moderationLabels[detail.moderation], tone: moderationTones[detail.moderation] });
    if (detail.report_count > 0) pills.push({ text: `被举报 ${detail.report_count} 次`, tone: "bad" });
    if (detail.owner_banned) pills.push({ text: "作者已封禁", tone: "bad" });
    fields.push(
      { label: "作者", value: detail.author || "—" },
      { label: "提交时间", value: formatTime(detail.created_at) },
      { label: "数据", value: dataField(detail) },
      { label: "版本", value: detail.version ? `v${detail.version}` : detail.revision ? `第 ${detail.revision} 版` : "—" },
      ...sectionFields(section, detail),
    );
    if ((section === "skins" || section === "candidate-skins") && detail.category && isCandidateCategory(detail.category)) {
      const from = detail.category;
      fields.push({ label: "分类", value: <CategorySelect value={from} disabled={!canReview} onChange={to => onCategory(item, from, to)} /> });
    }
    if (detail.moderated_by) fields.push({ label: "审核人", value: detail.moderated_by, mono: true }, { label: "审核时间", value: formatTime(detail.moderated_at) });
    if (detail.moderation === "removed" && detail.moderation_reason) fields.push({ label: "下架原因", value: detail.moderation_reason === "owner_banned" ? "作者账号被封禁" : detail.moderation_reason });
    if (detail.description) sections.push({ title: "简介", items: [{ text: detail.description }] });
    const content = contentSection(section, detail);
    if (content) sections.push(content);
    sections.push(
      { title: "举报记录", empty: "没有举报", items: detail.reports.map(report => ({ text: report.detail ? `${report.reason}：${report.detail}` : report.reason, meta: `${report.reporter} · ${relativeTime(report.created_at)}` })) },
      { title: "自动检查", empty: "未发现问题", items: detail.flags.map(flag => ({ text: `命中敏感词「${flag.pattern}」`, meta: `${sensitiveCategoryLabels[flag.category] ?? flag.category} · ${sensitiveLevelLabels[flag.level] ?? flag.level}` })) },
      { title: "作者其他作品", empty: "暂无", items: detail.owner_items.map(other => ({ text: `${sectionLabels[other.section]}「${other.name}」`, meta: moderationLabels[other.moderation] })) },
    );
    // A banned author's removed work comes back only when the account is unbanned on the users page.
    const bannedRemoval = detail.moderation === "removed" && detail.owner_banned;
    const republish = !canReview || bannedRemoval;
    if (detail.moderation === "removed") {
      actions.push({ label: "恢复", disabled: republish, onClick: () => onRestore(item) });
    } else {
      actions.push({ label: detail.moderation === "approved" ? "下架" : "驳回", variant: "danger", disabled: !canReview, onClick: () => onRemove(item) });
    }
    if (detail.moderation !== "approved") actions.push({ label: "通过并上架", variant: "primary", disabled: republish, onClick: () => onApprove(item) });
  }
  return <DetailDrawer open={Boolean(target)} onClose={onClose}
    title={detail?.name ?? "加载中…"} sub={detail ? `${sectionLabels[section]} · ${detail.author || "—"}` : sectionLabels[section]}
    pills={pills} fields={fields} sections={sections} actions={actions}>
    {query.isPending && <SkeletonRows rows={6} className="p-0" />}
    {query.isError && <ErrorState error={query.error} onRetry={() => query.refetch()} className="mb-5" />}
    {detail && section === "skins" && <KeyboardPreview design={detail.content} name={detail.name} />}
    {detail && section === "candidate-skins" && <CandidatePreview id={detail.id} name={detail.name} />}
  </DetailDrawer>;
}

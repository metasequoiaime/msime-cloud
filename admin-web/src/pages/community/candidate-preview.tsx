import { useEffect, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { useAPI } from "../../api/client";
import { contentPreviewSchema } from "../../api/community";
import type { Detail } from "../../api/community";
import { keys } from "../../api/keys";
import { CandidateArtwork } from "../../candidate-preview";
import { parseCandidateDesign } from "../../candidate-design";
import { useAPIImage } from "../../hooks/use-api-image";
import { Skeleton } from "../../ui/states";
import { usePreviewVisible } from "./use-preview-visible";

function UploadedPreview({ id, name, revision, enabled }: { id: string; name: string; revision?: string | null; enabled: boolean }) {
  const image = useAPIImage({
    queryKey: keys.page("community", "preview", id, revision),
    path: `/api/candidate-skins/${encodeURIComponent(id)}/preview`, enabled,
  });
  return image.data ? <img src={image.data} alt={`${name}上传的预览图`} className="mt-2 block max-h-[360px] max-w-full object-contain" />
    : <p className="text-xs text-muted">{image.isError ? "预览图加载失败" : "正在读取预览图…"}</p>;
}

// 列表读取真实清单，详情直接使用已读到的清单；两处都不把作者上传的截图当成实时外观。
export function CandidatePreview({ id, name, revision, manifest, compact = false }: { id: string; name: string; revision?: string | null; manifest?: string; compact?: boolean }) {
  const { container, visible } = usePreviewVisible(!compact);
  const api = useAPI();
  const [uploaded, setUploaded] = useState(false);
  const detail = useQuery({
    queryKey: keys.page("community", "candidate-preview", id, revision), enabled: visible && manifest === undefined, staleTime: 5 * 60_000,
    queryFn: ({ signal }) => api.get(`candidate-skins/${encodeURIComponent(id)}?view=preview`, contentPreviewSchema, { signal }),
  });
  const content = manifest ?? detail.data?.content;
  const design = typeof content === "string" ? parseCandidateDesign(content) : null;
  return <div ref={container} className={compact ? "h-full" : "mb-5"}>
    {design ? <CandidateArtwork design={design} name={name} compact={compact} source={{ id, revision }} />
      : manifest === undefined && detail.isPending ? <Skeleton className="h-[100px] w-full rounded-lg" />
        : <p role="status" className="m-0 rounded-xl bg-panel-2 p-3 text-sm text-muted">{detail.isError ? "清单加载失败" : "清单格式暂不支持预览，请检查 skin.toml。"}</p>}
    {!compact && <details onToggle={event => setUploaded(event.currentTarget.open)}><summary className="cursor-pointer text-xs text-muted">作者上传的预览图</summary><UploadedPreview id={id} name={name} revision={revision} enabled={uploaded} /></details>}
  </div>;
}

export function CandidateDraftPreview({ name, manifest, manifestFile, files, detail }: {
  name: string; manifest: string; manifestFile: File | null; files: { file: File; path: string }[]; detail?: Detail;
}) {
  const [imported, setImported] = useState<{ file: File; text?: string; error?: boolean }>();
  useEffect(() => {
    if (!manifestFile) return;
    let cancelled = false;
    if (manifestFile.size > 65536) { setImported({ file: manifestFile, error: true }); return; }
    void manifestFile.text().then(text => { if (!cancelled) setImported({ file: manifestFile, text }); }, () => { if (!cancelled) setImported({ file: manifestFile, error: true }); });
    return () => { cancelled = true; };
  }, [manifestFile]);
  const source = manifestFile ? imported?.file === manifestFile ? imported.text ?? "" : "" : manifest;
  const design = parseCandidateDesign(source);
  return <section aria-label="候选窗皮肤实时预览">
    {design ? <CandidateArtwork design={design} name={name || "新皮肤"} source={{ id: detail?.id, revision: detail?.edit_token ?? detail?.updated_at, files }} />
      : <p role="status" className="m-0 rounded-xl bg-panel-2 p-3 text-sm text-muted">{manifestFile && imported?.file !== manifestFile ? "正在读取清单…" : "请填写有效清单后预览候选窗效果。"}</p>}
  </section>;
}

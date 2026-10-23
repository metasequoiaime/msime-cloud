import { cloneElement, useId, useRef, useState } from "react";
import type { ReactElement } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { errorMessage, useAPI } from "../../api/client";
import { invalidateQueries } from "../../api/cache";
import type { Detail, Section } from "../../api/community";
import { candidateCategories, candidateCategoryLabels, moderationLabels } from "../../api/community";
import { keys } from "../../api/keys";
import { usersSchema } from "../../api/users";
import { useDebounced } from "../../hooks/use-debounced";
import { Button } from "../../ui/button";
import { KeyboardPreview } from "../../ui/keyboard-preview";
import { ModalFrame, ModalHeading } from "../../ui/modal";
import { ErrorState } from "../../ui/states";
import { useToast } from "../../ui/toast";
import { CandidateDraftPreview } from "./candidate-preview";

export type EditableSection = Extract<Section, "skins" | "candidate-skins" | "plugins">;
export const isEditableSection = (section: Section): section is EditableSection => section === "skins" || section === "candidate-skins" || section === "plugins";
const labels = { skins: "键盘皮肤", "candidate-skins": "候选窗皮肤", plugins: "插件" };
const actionKinds = { skins: "skin", "candidate-skins": "candidate_skin", plugins: "plugin" };
const inputClass = "block w-full rounded-[10px] bg-panel-2 px-3 py-2 text-[13.5px] text-ink outline-none focus:ring-[1.5px] focus:ring-accent";
const defaultDesign = { background: 15266027, keyBackground: 16777215, keyForeground: 1516829, accent: 1596487, actionBackground: 1596487, cornerRadius: 8, borderWidth: 0, shadow: 0, pattern: 0, monospaced: false };
const colorFields = { background: "背景颜色", keyBackground: "按键颜色", keyForeground: "文字颜色", accent: "强调颜色", actionBackground: "功能键颜色" };

function Field({ label, children }: { label: string; children: ReactElement<{ id?: string }> }) {
  const id = useId();
  return <div className="grid gap-1.5 text-[13px] text-body"><label htmlFor={id}>{label}</label>{cloneElement(children, { id })}</div>;
}

async function base64(file: File, limit: number): Promise<string> {
  if (file.size > limit) throw new Error(`「${file.name}」超过文件大小限制。`);
  const bytes = new Uint8Array(await file.arrayBuffer());
  let binary = "";
  for (let offset = 0; offset < bytes.length; offset += 8192) binary += String.fromCharCode(...bytes.subarray(offset, offset + 8192));
  return btoa(binary);
}

export function ContentEditor({ section, detail, onClose, onSaved }: { section: EditableSection; detail?: Detail; onClose: () => void; onSaved: (id: string) => void }) {
  const api = useAPI();
  const client = useQueryClient();
  const toast = useToast();
  const [id] = useState(() => detail?.id ?? crypto.randomUUID());
  const [name, setName] = useState(detail?.name ?? "");
  const [description, setDescription] = useState(detail?.description ?? "");
  const [owner, setOwner] = useState(detail?.owner_id ?? "");
  const [authorSearch, setAuthorSearch] = useState("");
  const debouncedAuthor = useDebounced(authorSearch.trim(), 250);
  const [category, setCategory] = useState(detail?.category ?? "other");
  const [moderation, setModeration] = useState(detail?.moderation ?? "removed");
  const [visibility, setVisibility] = useState(detail?.visibility ?? "public");
  const [design, setDesign] = useState(() => JSON.stringify(detail?.content ?? defaultDesign, null, 2));
  const [manifest, setManifest] = useState(typeof detail?.content === "string" ? detail.content : "");
  const [manifestFile, setManifestFile] = useState<File | null>(null);
  const [archive, setArchive] = useState<File | null>(null);
  const [files, setFiles] = useState<{ file: File; path: string }[]>([]);
  const submitting = useRef(false);
  const users = useQuery({
    queryKey: keys.page("community", "authors", debouncedAuthor), enabled: !detail,
    queryFn: ({ signal }) => api.get(`users?q=${encodeURIComponent(debouncedAuthor)}`, usersSchema, { signal }),
  });
  let parsed: Record<string, unknown> | undefined;
  try {
    const value: unknown = JSON.parse(design);
    if (value && typeof value === "object" && !Array.isArray(value)) parsed = value as Record<string, unknown>;
  } catch { /* 编辑过程中的不完整数据留在文本框中，保存时再提示。 */ }
  const save = useMutation({
    mutationFn: async () => {
      const value: Record<string, unknown> = { owner_id: owner, name: name.trim(), description: description.trim(), moderation, ...(detail ? { edit_token: detail.edit_token } : {}) };
      if (section === "skins") {
        if (!parsed) throw new Error("皮肤设计必须是有效的 JSON 对象。");
        Object.assign(value, { design: parsed, category });
      } else if (section === "plugins") {
        if (!detail && !archive) throw new Error("请选择插件 ZIP 包。");
        if (archive) value.archive = await base64(archive, 8 * 1024 * 1024);
      } else {
        if (manifestFile && manifestFile.size > 65536) throw new Error("皮肤清单不能超过 64 KiB。");
        Object.assign(value, { category, visibility, manifest: manifestFile ? await manifestFile.text() : manifest });
        if (files.length) {
          if (files.length > 37 || files.reduce((sum, entry) => sum + entry.file.size, 0) > 2 * 1024 * 1024) throw new Error("候选窗皮肤最多 37 张图片，总大小不超过 2 MiB。");
          const images: Record<string, string> = {};
          for (const { file, path } of files) {
            if (Object.hasOwn(images, path)) throw new Error("图片路径重复，请重新选择。");
            images[path] = await base64(file, 1024 * 1024);
          }
          value.files = images;
        }
        if (!detail && files.length === 0) throw new Error("请选择清单引用的皮肤图片。");
      }
      await api.action({ action: `${detail ? "update" : "create"}_${actionKinds[section]}`, section, id, value });
    },
    onSuccess: () => {
      void invalidateQueries(client, keys.page("community"), keys.shell, keys.page("users"));
      toast(`「${name.trim()}」已保存`);
      onSaved(id);
    },
    onSettled: () => { submitting.current = false; },
  });
  const close = () => { if (!save.isPending && !submitting.current) onClose(); };
  const updateDesign = (key: string, value: unknown) => setDesign(JSON.stringify({ ...parsed, [key]: value }, null, 2));
  return <ModalFrame wide onClose={close}>
    <ModalHeading title={`${detail ? "编辑" : "新增"}${labels[section]}`} description={detail ? "修改作品内容并保存；作者保持不变。" : "选择作者并填写内容，可先保存为下架，检查后再上架。"} />
    <form onSubmit={event => { event.preventDefault(); if (submitting.current) return; submitting.current = true; save.mutate(); }}>
      <fieldset disabled={save.isPending} className="m-0 mt-4 grid max-h-[65vh] gap-3 overflow-y-auto border-0 p-0 pr-1 disabled:opacity-70">
        {detail ? <p className="m-0 text-[13px] text-muted">作者：{detail.author || "未命名用户"}</p> : <>
          <Field label="搜索作者"><input className={inputClass} value={authorSearch} maxLength={100} onChange={event => { setAuthorSearch(event.target.value); setOwner(""); }} placeholder="按昵称或账号搜索…" /></Field>
          <Field label="作者账号"><select className={inputClass} required value={owner} onChange={event => setOwner(event.target.value)}>
            <option value="">{users.isPending ? "正在加载作者…" : "请选择作者"}</option>
            {users.data?.items.filter(user => !user.banned).map(user => <option key={user.id} value={user.id}>{user.display_name || "未命名用户"}{user.contact ? ` · ${user.contact}` : ""}</option>)}
          </select></Field>
          {users.isError && <ErrorState error={users.error} onRetry={() => users.refetch()} />}
          {users.data?.has_more && <p className="m-0 text-xs text-muted">仅显示前 50 个账号，请搜索缩小范围。</p>}
        </>}
        <Field label="名称"><input required className={inputClass} maxLength={32} value={name} onChange={event => setName(event.target.value)} /></Field>
        <Field label="简介"><textarea className={inputClass} maxLength={280} rows={2} value={description} onChange={event => setDescription(event.target.value)} /></Field>
        <div className="grid grid-cols-2 gap-3">
          <Field label="发布状态"><select className={inputClass} value={moderation} onChange={event => setModeration(event.target.value as typeof moderation)}>{(["removed", "approved", "pending"] as const).map(state => <option key={state} value={state}>{moderationLabels[state]}</option>)}</select></Field>
          {section !== "plugins" && <Field label="图库分类"><select className={inputClass} value={category} onChange={event => setCategory(event.target.value)}>{candidateCategories.map(key => <option key={key} value={key}>{candidateCategoryLabels[key]}</option>)}</select></Field>}
        </div>
        {section === "skins" && <>
          <div className="grid grid-cols-2 gap-3">{Object.entries(colorFields).map(([key, label]) => <Field key={key} label={label}><input type="color" className="h-9 w-full cursor-pointer rounded-lg" disabled={!parsed} value={`#${Number(parsed?.[key] ?? 0).toString(16).padStart(6, "0")}`} onChange={event => updateDesign(key, Number.parseInt(event.target.value.slice(1), 16))} /></Field>)}
            <Field label="圆角半径"><input type="number" className={inputClass} min={0} max={20} step={0.5} disabled={!parsed} value={Number(parsed?.cornerRadius ?? 8)} onChange={event => updateDesign("cornerRadius", Number(event.target.value))} /></Field>
          </div>
          {parsed && <KeyboardPreview design={parsed} name={name || "新皮肤"} />}
          <details><summary className="cursor-pointer text-[13px] text-muted">完整设计参数（JSON）</summary><Field label="皮肤设计"><textarea className={`${inputClass} mt-2 font-mono text-xs`} rows={8} value={design} onChange={event => setDesign(event.target.value)} /></Field></details>
        </>}
        {section === "plugins" && <>
          <Field label={detail ? "替换插件包（可选）" : "插件 ZIP 包"}><input type="file" accept=".zip" required={!detail} onChange={event => setArchive(event.target.files?.[0] ?? null)} className={inputClass} /></Field>
          <p className="m-0 text-xs leading-relaxed text-muted">ZIP 不超过 8 MiB，类型、版本及许可从包内的 plugin.toml 读取。{detail && "不选新包时保留当前插件内容。"}</p>
        </>}
        {section === "candidate-skins" && <>
          <Field label="可见性"><select className={inputClass} value={visibility} onChange={event => setVisibility(event.target.value as typeof visibility)}><option value="public">公开图库</option><option value="private">作者私有库</option></select></Field>
          <Field label="导入 skin.toml（可选）"><input type="file" accept=".toml" className={inputClass} onChange={event => setManifestFile(event.target.files?.[0] ?? null)} /></Field>
          <Field label="皮肤清单（skin.toml）"><textarea required={!manifestFile} disabled={Boolean(manifestFile)} rows={8} className={`${inputClass} font-mono text-xs`} value={manifestFile ? `将使用文件「${manifestFile.name}」的内容` : manifest} onChange={event => setManifest(event.target.value)} /></Field>
          <Field label={detail ? "替换全部图片（可选）" : "皮肤图片"}><input type="file" multiple accept=".png,.jpg,.jpeg,.apng,.gif,.webp" required={!detail} className={inputClass} onChange={event => setFiles(Array.from(event.target.files ?? []).map(file => ({ file, path: detail?.files?.find(existing => existing.path.split("/").at(-1) === file.name)?.path ?? file.name })))} /></Field>
          {files.map((entry, index) => <Field key={entry.file.name} label={`${entry.file.name} 的包内路径`}><input required className={inputClass} value={entry.path} onChange={event => setFiles(current => current.map((item, at) => at === index ? { ...item, path: event.target.value } : item))} /></Field>)}
          <CandidateDraftPreview name={name} manifest={manifest} manifestFile={manifestFile} files={files} detail={detail} />
          <p className="m-0 text-xs leading-relaxed text-muted">选择清单引用的全部图片，文件名须与清单一致。支持 PNG、JPEG、APNG、GIF 和 WebP，最多 37 张，每张不超过 1 MiB，总共不超过 2 MiB；预览图仍须使用 PNG/JPEG。{detail && "不选新图片时保留现有图片。"}</p>
        </>}
      </fieldset>
      {save.isError && <p role="alert" className="mt-3 text-sm text-bad">{errorMessage(save.error)}</p>}
      <div className="mt-5 flex justify-end gap-2"><Button variant="outline" disabled={save.isPending} onClick={close}>取消</Button><Button type="submit" variant="primary" disabled={save.isPending || (!detail && !owner)}>{save.isPending ? "保存中…" : "保存"}</Button></div>
    </form>
  </ModalFrame>;
}

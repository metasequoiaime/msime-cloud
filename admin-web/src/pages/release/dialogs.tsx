import { useState } from "react";
import type { FormEvent, ReactNode } from "react";
import { Dialog } from "radix-ui";
import { Button } from "../../ui/button";
import { keepOpenForToast, useRegisterOverlay } from "../../ui/overlay";

// FormDialog is the release page's modal form (trigger version, notes editor). Like the shared confirm dialog it has no Dialog.Overlay, whose scroll lock injects a <style> element the admin CSP blocks.
function FormDialog({ title, description, okLabel, okDisabled, pending, onClose, onSubmit, wide, children }: {
  title: string;
  okDisabled?: boolean;
  description?: ReactNode;
  okLabel: string;
  pending: boolean;
  onClose: () => void;
  onSubmit: () => void;
  wide?: boolean;
  children: ReactNode;
}) {
  useRegisterOverlay(true, () => { if (!pending) onClose(); });
  const submit = (event: FormEvent) => {
    event.preventDefault();
    if (!pending && !okDisabled) onSubmit();
  };
  return <Dialog.Root open onOpenChange={open => { if (!open && !pending) onClose(); }}>
    <Dialog.Portal>
      <div className="fixed inset-0 z-70 grid place-items-center bg-[rgba(10,20,14,.38)] p-5">
        <Dialog.Content onInteractOutside={keepOpenForToast} className={`w-full animate-pop-in rounded-[18px] bg-panel px-6 py-[22px] shadow-dialog outline-none ${wide ? "max-w-[640px]" : "max-w-[400px]"}`}>
          <form onSubmit={submit}>
            <Dialog.Title className="m-0 text-[17px] font-bold text-ink">{title}</Dialog.Title>
            {description ? <Dialog.Description className="m-0 mt-2 text-sm leading-[1.8] text-body">{description}</Dialog.Description> : <Dialog.Description className="sr-only">{title}</Dialog.Description>}
            <div className="mt-4">{children}</div>
            <div className="mt-5 flex justify-end gap-2">
              <Button variant="outline" onClick={onClose} disabled={pending}>取消</Button>
              <Button type="submit" variant="primary" disabled={pending || okDisabled}>{pending ? "提交中…" : okLabel}</Button>
            </div>
          </form>
        </Dialog.Content>
      </div>
    </Dialog.Portal>
  </Dialog.Root>;
}

const versionPattern = /^v?[0-9][0-9A-Za-z.+-]{0,62}$/;

// TriggerDialog asks for the version the release workflow is dispatched with, prefilled with the platform's current draft or prerelease.
export function TriggerDialog({ platform, workflow, initialVersion, pending, onClose, onSubmit }: {
  platform: string;
  workflow: string;
  initialVersion: string;
  pending: boolean;
  onClose: () => void;
  onSubmit: (version: string) => void;
}) {
  const [version, setVersion] = useState(initialVersion);
  const valid = versionPattern.test(version.trim());
  return <FormDialog title={`触发 ${platform} 发布`} okLabel="触发发布" okDisabled={!valid} pending={pending} onClose={onClose} onSubmit={() => onSubmit(version.trim())}
    description={<>在默认分支上运行 <span className="font-mono text-[13px]">{workflow}</span>，流水线完成后 GitHub Release 会出现在发布历史里。</>}>
    <label className="block text-[12.5px] font-semibold text-muted" htmlFor="release-version">版本号</label>
    {/* biome-ignore lint/a11y/noAutofocus: the dialog exists to collect this one value */}
    <input id="release-version" autoFocus disabled={pending} value={version} onChange={event => setVersion(event.target.value)} placeholder="v0.5.5" spellCheck={false} autoComplete="off"
      className="mt-1.5 block h-9 w-full rounded-[10px] bg-panel-2 px-3 font-mono text-[13.5px] text-ink outline-none placeholder:text-muted focus:ring-[1.5px] focus:ring-accent disabled:cursor-wait disabled:opacity-60" />
    {version.trim() !== "" && !valid && <p className="m-0 mt-1.5 text-xs text-bad">版本号格式不正确，例如 v0.5.5。</p>}
  </FormDialog>;
}

// NotesDialog edits a release body; headings named 新增 / 修复 / 改进 / 说明 / 待办 become the tagged note lines.
export function NotesDialog({ title, initialBody, pending, onClose, onSubmit }: {
  title: string;
  initialBody: string;
  pending: boolean;
  onClose: () => void;
  onSubmit: (body: string) => void;
}) {
  const [body, setBody] = useState(initialBody);
  return <FormDialog wide title={title} okLabel="保存说明" pending={pending} onClose={onClose} onSubmit={() => onSubmit(body)}
    description={<>Markdown 格式，保存后直接更新 GitHub Release。用 <span className="font-mono text-[13px]">### 新增</span>、<span className="font-mono text-[13px]">### 修复</span>、<span className="font-mono text-[13px]">### 改进</span>、<span className="font-mono text-[13px]">### 说明</span>、<span className="font-mono text-[13px]">### 待办</span> 分组。</>}>
    <label className="sr-only" htmlFor="release-notes">发布说明</label>
    {/* biome-ignore lint/a11y/noAutofocus: the dialog exists to edit this text */}
    <textarea id="release-notes" autoFocus disabled={pending} rows={14} value={body} onChange={event => setBody(event.target.value)} spellCheck={false}
      placeholder={"### 新增\n- 剪贴板历史支持固定条目\n\n### 修复\n- Win11 24H2 下候选窗偏移"}
      className="block max-h-[60vh] w-full resize-y rounded-[10px] bg-panel-2 px-3 py-2.5 font-mono text-[13px] leading-relaxed text-ink outline-none placeholder:text-muted focus:ring-[1.5px] focus:ring-accent disabled:cursor-wait disabled:opacity-60" />
  </FormDialog>;
}

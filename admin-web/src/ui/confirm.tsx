import { createContext, useCallback, useContext, useRef, useState } from "react";
import type { KeyboardEvent, ReactNode } from "react";
import { ToggleGroup } from "radix-ui";
import { Button } from "./button";
import { ModalFrame, ModalHeading } from "./modal";

export type ConfirmOptions = {
  title: string;
  description?: ReactNode;
  // okLabel defaults to 确定.
  okLabel?: string;
  // reasons turns on the reason picker: the first preset is preselected and a free-text note can be appended.
  reasons?: readonly string[];
  // danger (default) renders the confirm button in --bad, primary in --btn.
  tone?: "danger" | "primary";
};

// confirm resolves to null when cancelled. When confirmed it resolves to the reason string (pick + '：' + note, or just pick without a note), or "" when no reasons were requested.
export type Confirm = (options: ConfirmOptions) => Promise<string | null>;

type Pending = ConfirmOptions & { id: number; resolve: (value: string | null) => void };

let nextID = 0;

const ConfirmContext = createContext<Confirm | null>(null);

export function ConfirmProvider({ children }: { children: ReactNode }) {
  const [pending, setPending] = useState<Pending | null>(null);
  const confirm = useCallback<Confirm>(options => new Promise(resolve => {
    setPending(previous => {
      previous?.resolve(null);
      return { ...options, id: ++nextID, resolve };
    });
  }), []);
  return <ConfirmContext.Provider value={confirm}>
    {children}
    {pending && <ConfirmDialog key={pending.id} pending={pending} onDone={value => { pending.resolve(value); setPending(null); }} />}
  </ConfirmContext.Provider>;
}

export function useConfirm(): Confirm {
  const value = useContext(ConfirmContext);
  if (!value) throw new Error("ConfirmProvider required");
  return value;
}

function ConfirmDialog({ pending, onDone }: { pending: Pending; onDone: (value: string | null) => void }) {
  const { title, description, okLabel = "确定", reasons, tone = "danger" } = pending;
  const [pick, setPick] = useState(reasons?.[0] ?? "");
  const [note, setNote] = useState("");
  const okRef = useRef<HTMLButtonElement>(null);
  const submit = () => {
    const trimmed = note.trim();
    onDone(pick ? pick + (trimmed ? `：${trimmed}` : "") : "");
  };
  // 焦点不在备注文本框或会自行触发的按钮上时，按 Enter 确认。
  const onKeyDown = (event: KeyboardEvent<HTMLDivElement>) => {
    const tag = (event.target as HTMLElement).tagName;
    if (event.key === "Enter" && tag !== "TEXTAREA" && tag !== "BUTTON" && !event.nativeEvent.isComposing) {
      event.preventDefault();
      submit();
    }
  };
  return <ModalFrame onClose={() => onDone(null)} onKeyDown={onKeyDown} onOpenAutoFocus={event => { event.preventDefault(); okRef.current?.focus(); }}>
    <ModalHeading title={title} description={description} />
    {reasons && reasons.length > 0 && <div className="mt-4">
      <div className="mb-2 text-[12.5px] font-semibold text-muted" id="confirm-reason-label">原因</div>
      <ToggleGroup.Root type="single" value={pick} onValueChange={next => { if (next) setPick(next); }} aria-labelledby="confirm-reason-label" className="flex flex-wrap gap-1.5">
        {reasons.map(reason => <ToggleGroup.Item key={reason} value={reason}
          className="h-[30px] rounded-full px-3 text-[13px] text-body inset-ring inset-ring-hair-2 transition hover:bg-panel-2 data-[state=on]:bg-accent-soft data-[state=on]:text-accent-ink data-[state=on]:inset-ring-accent-ring">{reason}</ToggleGroup.Item>)}
      </ToggleGroup.Root>
      <textarea rows={2} value={note} onChange={event => setNote(event.target.value)} placeholder="补充说明（可选）" aria-label="补充说明"
        className="mt-2.5 block w-full resize-y rounded-[10px] bg-panel-2 px-3 py-2 text-[13.5px] text-ink outline-none placeholder:text-muted focus:ring-[1.5px] focus:ring-accent" />
    </div>}
    <div className="mt-5 flex justify-end gap-2">
      <Button variant="outline" onClick={() => onDone(null)}>取消</Button>
      <Button ref={okRef} variant={tone === "danger" ? "danger" : "primary"} onClick={submit}>{okLabel}</Button>
    </div>
  </ModalFrame>;
}

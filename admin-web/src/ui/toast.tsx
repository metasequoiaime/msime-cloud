import { createContext, useCallback, useContext, useEffect, useRef, useState } from "react";
import type { ReactNode } from "react";
import { Toast } from "radix-ui";
import { errorMessage } from "../api/client";
import { cn } from "./cn";
import { TOAST_ATTRIBUTE, closeTopOverlay } from "./overlay";

const TOAST_MS = 4000;

export type ToastOptions = {
  text: string;
  // undo shows 撤销. For a reversible action that already ran, undo calls the reverse action. Combined with delayCommit, undo only needs to restore local optimistic state because the request is never sent.
  undo?: () => unknown;
  // delayCommit defers an irreversible request (merging a PR) for 4s. 撤销 cancels it; the timer, a newer toast with its own undo or delayCommit, logout (useFlushToast) or pagehide flushes it. A plain toast shown meanwhile appears beside it and leaves it waiting. On pagehide it is called with keepalive: true and must pass that to requestAPI so the request survives the unload.
  delayCommit?: (options: { keepalive: boolean }) => Promise<unknown>;
  // onCommitError runs when the delayed commit fails, after the failure toast is shown; use it to roll back optimistic state.
  onCommitError?: (error: unknown) => void;
};

export type ShowToast = (options: ToastOptions | string) => void;

// FlushToast 立即发出待提交的操作，并等待所有已发出的延迟提交完成（包括失败提示及 `onCommitError`），供退出登录使用。
export type FlushToast = () => Promise<void>;

type Current = ToastOptions & { id: number };
type PendingCommit = { id: number; run: (keepalive: boolean) => Promise<void>; timer: number };

const ToastContext = createContext<ShowToast | null>(null);
const FlushContext = createContext<FlushToast | null>(null);

export function ToastProvider({ children }: { children: ReactNode }) {
  const [current, setCurrent] = useState<Current | null>(null);
  const [open, setOpen] = useState(false);
  // notice is a second slot for a plain toast shown while a delayed commit waits, so the commit keeps its 4s and its 撤销 stays on screen.
  const [notice, setNotice] = useState<Current | null>(null);
  const [noticeOpen, setNoticeOpen] = useState(false);
  const pending = useRef<PendingCommit | null>(null);
  const inFlight = useRef(new Set<Promise<void>>());
  const hideTimer = useRef(0);
  const noticeTimer = useRef(0);
  const nextID = useRef(0);
  const shownID = useRef(0);
  const noticeID = useRef(0);

  const flush = useCallback((keepalive: boolean): Promise<void> => {
    const commit = pending.current;
    if (commit) {
      pending.current = null;
      window.clearTimeout(commit.timer);
      // 请求已经发出，不再允许撤销；保留它的 `Promise`，退出时也要等待计时器或其他提示已发出的操作。
      if (shownID.current === commit.id) setOpen(false);
      const running = commit.run(keepalive);
      inFlight.current.add(running);
      const settled = () => { inFlight.current.delete(running); };
      void running.then(settled, settled);
    }
    return Promise.all([...inFlight.current]).then(() => undefined);
  }, []);

  const show = useCallback<ShowToast>(input => {
    const options = typeof input === "string" ? { text: input } : input;
    const id = ++nextID.current;
    // Only a newer undoable action replaces a toast whose delayed commit is still waiting. Anything else (a confirmation, another request's failure) goes to the notice slot and must not send someone else's irreversible request early.
    if (pending.current && !options.delayCommit && !options.undo) {
      noticeID.current = id;
      setNotice({ ...options, id });
      setNoticeOpen(true);
      window.clearTimeout(noticeTimer.current);
      noticeTimer.current = window.setTimeout(() => setNoticeOpen(false), TOAST_MS);
      return;
    }
    // A newer undoable toast replaces the old one; an unfinished delayed commit of the old toast is sent now rather than dropped.
    void flush(false);
    shownID.current = id;
    setCurrent({ ...options, id });
    setOpen(true);
    window.clearTimeout(hideTimer.current);
    hideTimer.current = window.setTimeout(() => setOpen(false), TOAST_MS);
    const { delayCommit } = options;
    if (delayCommit) {
      const run = (keepalive: boolean) => delayCommit({ keepalive }).then(() => undefined, error => {
        show(`操作失败：${errorMessage(error)}`);
        options.onCommitError?.(error);
      });
      pending.current = { id, run, timer: window.setTimeout(() => void flush(false), TOAST_MS) };
    }
  }, [flush]);

  const flushNow = useCallback<FlushToast>(() => flush(false), [flush]);

  useEffect(() => {
    const onPageHide = () => void flush(true);
    window.addEventListener("pagehide", onPageHide);
    return () => {
      window.removeEventListener("pagehide", onPageHide);
      window.clearTimeout(hideTimer.current);
      window.clearTimeout(noticeTimer.current);
      void flush(true);
    };
  }, [flush]);

  const undo = () => {
    if (!current) return;
    const commit = pending.current;
    if (commit && commit.id === current.id) {
      window.clearTimeout(commit.timer);
      pending.current = null;
    }
    const result = current.undo?.();
    if (result instanceof Promise) {
      result.then(() => show("已撤销"), error => show(`撤销失败：${errorMessage(error)}`));
    } else {
      show("已撤销");
    }
  };

  const canUndo = Boolean(current && (current.undo || current.delayCommit));
  return <ToastContext.Provider value={show}>
    <FlushContext.Provider value={flushNow}>
      <Toast.Provider duration={Number.POSITIVE_INFINITY} swipeDirection="down" label="通知">
        {children}
        {current && <ToastView key={current.id} toast={current} open={open} className="bottom-7"
          // 撤销 closes its own toast after its click handler already showed 已撤销; ignore close events from a toast that has been replaced.
          onOpenChange={next => { if (!next && shownID.current === current.id) setOpen(false); }}>
          {canUndo && <Toast.Action altText="撤销刚才的操作" onClick={undo} className="shrink-0 font-bold text-bg underline underline-offset-2">撤销</Toast.Action>}
        </ToastView>}
        {notice && <ToastView key={notice.id} toast={notice} open={noticeOpen} className="bottom-[84px]"
          onOpenChange={next => { if (!next && noticeID.current === notice.id) setNoticeOpen(false); }} />}
        <Toast.Viewport className="fixed bottom-0 left-0 z-80 m-0 list-none p-0 outline-none" />
      </Toast.Provider>
    </FlushContext.Provider>
  </ToastContext.Provider>;
}

function ToastView({ toast, open, className, onOpenChange, children }: { toast: Current; open: boolean; className: string; onOpenChange: (open: boolean) => void; children?: ReactNode }) {
  return <Toast.Root open={open} type="foreground" {...{ [TOAST_ATTRIBUTE]: "" }} onOpenChange={onOpenChange}
    // A toast shown after a dialog or drawer opened is the topmost dismissable layer and would take Escape for itself; the key closes that overlay instead and the toast stays for its 撤销.
    onEscapeKeyDown={event => { if (closeTopOverlay()) event.preventDefault(); }}
    // While a modal overlay disables outside pointer events, the toast stays clickable (!important beats the layer's inline pointer-events: none) so 撤销 still works; the overlays ignore that click through keepOpenForToast.
    className={cn("pointer-events-auto! fixed left-1/2 z-80 flex max-w-[calc(100vw-32px)] animate-toast-in items-center gap-3.5 rounded-xl bg-ink px-[18px] py-3 text-sm text-bg shadow-dialog", className)}>
    <Toast.Description>{toast.text}</Toast.Description>
    {children}
  </Toast.Root>;
}

export function useToast(): ShowToast {
  const value = useContext(ToastContext);
  if (!value) throw new Error("ToastProvider required");
  return value;
}

export function useFlushToast(): FlushToast {
  const value = useContext(FlushContext);
  if (!value) throw new Error("ToastProvider required");
  return value;
}

import { useEffect, useRef, useSyncExternalStore } from "react";
import { createListeners } from "../utils/listeners";

// Tracks open modal overlays (confirm dialog, detail drawer, release dialogs): page hotkeys stay inert while one is open, matching the prototype where the confirm dialog swallows every key but Enter and Escape, and the toast hands Escape to the topmost one.
type Entry = { close: () => void };
const stack: Entry[] = [];
const listeners = createListeners();

// useRegisterOverlay marks an overlay as open while active is true; onClose is how Escape closes it when another layer (a toast shown after it opened) receives the key first.
export function useRegisterOverlay(active: boolean, onClose: () => void) {
  const close = useRef(onClose);
  close.current = onClose;
  useEffect(() => {
    if (!active) return;
    const entry: Entry = { close: () => close.current() };
    stack.push(entry);
    listeners.emit();
    return () => {
      const index = stack.indexOf(entry);
      if (index >= 0) stack.splice(index, 1);
      listeners.emit();
    };
  }, [active]);
}

export function overlayOpen(): boolean {
  return stack.length > 0;
}

export function useOverlayOpen(): boolean {
  return useSyncExternalStore(listeners.subscribe, overlayOpen);
}

// closeTopOverlay closes the most recently opened overlay and reports whether there was one.
export function closeTopOverlay(): boolean {
  const top = stack.at(-1);
  if (!top) return false;
  top.close();
  return true;
}

// TOAST_ATTRIBUTE marks the toast element; overlays ignore outside interactions on it so its 撤销 works without dismissing them.
export const TOAST_ATTRIBUTE = "data-msime-toast";

export function keepOpenForToast(event: { target: EventTarget | null; preventDefault: () => void }) {
  if (event.target instanceof Element && event.target.closest(`[${TOAST_ATTRIBUTE}]`)) event.preventDefault();
}

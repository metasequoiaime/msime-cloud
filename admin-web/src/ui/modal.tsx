import type { ComponentProps, ReactNode } from "react";
import { Dialog } from "radix-ui";
import { keepOpenForToast, useRegisterOverlay } from "./overlay";

type ModalFrameProps = Pick<ComponentProps<typeof Dialog.Content>, "onKeyDown" | "onOpenAutoFocus"> & {
  onClose: () => void;
  wide?: boolean;
  children: ReactNode;
};

// 挂载即打开；调用方负责卸载，并在 onClose 中保留提交期间的关闭保护。
export function ModalFrame({ onClose, wide, children, onKeyDown, onOpenAutoFocus }: ModalFrameProps) {
  useRegisterOverlay(true, onClose);
  // 普通遮罩避免 Dialog.Overlay 的滚动锁注入被后台 CSP 禁止的 style 元素。
  return <Dialog.Root open onOpenChange={open => { if (!open) onClose(); }}>
    <Dialog.Portal>
      <div className="fixed inset-0 z-70 grid place-items-center bg-[rgba(10,20,14,.38)] p-5">
        <Dialog.Content onKeyDown={onKeyDown} onOpenAutoFocus={onOpenAutoFocus} onInteractOutside={keepOpenForToast}
          className={`w-full animate-pop-in rounded-[18px] bg-panel px-6 py-[22px] shadow-dialog outline-none ${wide ? "max-w-[640px]" : "max-w-[400px]"}`}>
          {children}
        </Dialog.Content>
      </div>
    </Dialog.Portal>
  </Dialog.Root>;
}

export function ModalHeading({ title, description }: { title: string; description?: ReactNode }) {
  return <>
    <Dialog.Title className="m-0 text-[17px] font-bold text-ink">{title}</Dialog.Title>
    {description ? <Dialog.Description className="m-0 mt-2 text-sm leading-[1.8] text-body">{description}</Dialog.Description> : <Dialog.Description className="sr-only">{title}</Dialog.Description>}
  </>;
}

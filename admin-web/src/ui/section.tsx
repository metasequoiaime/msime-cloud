import type { HTMLAttributes, ReactNode } from "react";
import { cn } from "./cn";

// 分组卡片共用标题、右侧操作区和贴边的分隔行。
export function SectionCard({ title, actions, children, className }: { title: ReactNode; actions?: ReactNode; children: ReactNode; className?: string }) {
  return <section className={cn("min-w-0 overflow-hidden rounded-[18px] bg-panel ring-1 ring-hair", className)}>
    <div className="flex items-center justify-between gap-3 border-b border-hair px-5 py-4">
      <h2 className="m-0 text-[15px] font-bold text-ink">{title}</h2>
      {actions && <div className="flex items-center gap-2">{actions}</div>}
    </div>
    {children}
  </section>;
}

// 分组卡片中的一行；末行不显示底部分隔线。
export function SectionRow({ className, ...props }: HTMLAttributes<HTMLDivElement>) {
  return <div className={cn("flex items-center gap-3 border-b border-hair px-5 py-[13px] last:border-b-0", className)} {...props} />;
}

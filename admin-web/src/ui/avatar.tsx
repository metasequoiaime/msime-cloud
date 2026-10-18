import { cn } from "./cn";

// 用姓名首字显示头像；后台 CSP 不允许读取外部头像图片。
export function Avatar({ letter, size = "sm" }: { letter: string; size?: "sm" | "lg" }) {
  return <span aria-hidden="true" className={cn("inline-flex shrink-0 items-center justify-center rounded-full font-bold",
    size === "lg" ? "h-16 w-16 bg-accent text-[26px] text-btn-fg" : "h-[30px] w-[30px] bg-accent-soft text-[12.5px] text-accent-ink")}>{letter}</span>;
}

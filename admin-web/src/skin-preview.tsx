import { useId, useState } from "react";
import { z } from "zod";
import { Delete, Globe, CornerDownLeft, ArrowUp } from "lucide-react";
import { Segmented } from "./ui/segmented";

const color = z.number().int().min(0).max(0xffffff);
const designSchema = z.object({
  background: color, keyBackground: color, keyForeground: color, accent: color, actionBackground: color,
  cornerRadius: z.number().min(0).max(20), borderWidth: z.number().min(0).max(2), shadow: z.number().min(0).max(.4), pattern: z.number().int().min(0).max(3), monospaced: z.boolean(),
  keyShape: z.enum(["", "rounded", "capsule", "ticket", "pebble"]).optional(), keyMaterial: z.enum(["", "flat", "raised", "glass", "paper"]).optional(),
  keyOpacity: z.number().min(.25).max(1).nullish(), gradientEnd: color.nullish(), gradientHorizontal: z.boolean().nullish(),
  patternOpacity: z.number().min(0).max(.5).nullish(), customBorderColor: color.nullish(),
  photo: z.string().max(683000).regex(/^[A-Za-z0-9+/]*={0,2}$/).nullish(), photoShade: z.number().min(0).max(.8).nullish(), photoPosition: z.number().min(0).max(1).nullish(),
});
const hex = (value: number) => `#${value.toString(16).padStart(6, "0")}`;
type Design = z.infer<typeof designSchema>;
const foreground = (value: number) => {
  const channel = (shift: number) => {
    const c = ((value >> shift) & 255) / 255;
    return c <= .04045 ? c / 12.92 : ((c + .055) / 1.055) ** 2.4;
  };
  return .2126 * channel(16) + .7152 * channel(8) + .0722 * channel(0) > .179 ? "#000000" : "#ffffff";
};

// 与客户端 SkinKeySurfaceView 的轮廓、材质和默认值保持一致。
function KeySurface({ design: d, width, action }: { design: Design; width: number; action: boolean }) {
  const id = useId();
  const w = width - 2, h = 41 - (d.keyMaterial === "raised" ? 3 : 0);
  const radius = d.keyShape === "capsule" ? Math.min(w, h) / 2 : Math.min(d.cornerRadius, w / 2, h / 2);
  const notch = Math.min(w, h) * .12;
  const path = d.keyShape === "pebble"
    ? `M${w * .35} 0C${w * .83} 0 ${w} ${h * .04} ${w} ${h * .3}C${w} ${h * .85} ${w * .9} ${h} ${w * .68} ${h}C${w * .18} ${h} 0 ${h * .97} 0 ${h * .7}C0 ${h * .2} ${w * .06} 0 ${w * .35} 0Z`
    : d.keyShape === "ticket"
      ? `M0 0H${w}V${h / 2 - notch}A${notch} ${notch} 0 0 0 ${w} ${h / 2 + notch}V${h}H0V${h / 2 + notch}A${notch} ${notch} 0 0 0 0 ${h / 2 - notch}Z`
      : `M${radius} 0H${w - radius}A${radius} ${radius} 0 0 1 ${w} ${radius}V${h - radius}A${radius} ${radius} 0 0 1 ${w - radius} ${h}H${radius}A${radius} ${radius} 0 0 1 0 ${h - radius}V${radius}A${radius} ${radius} 0 0 1 ${radius} 0Z`;
  const fill = hex(action ? d.actionBackground : d.keyBackground), opacity = action ? 1 : d.keyOpacity ?? 1;
  const raised = d.keyMaterial === "raised", glass = d.keyMaterial === "glass";
  return <g transform="translate(1 1)" data-key-surface="">
    <defs>
      <clipPath id={`${id}-face`}><path d={path} /></clipPath>
      <linearGradient id={`${id}-material`} x1="0" y1="0" x2="0" y2="1">
        <stop stopColor="#ffffff" stopOpacity={glass ? .24 : .13} /><stop offset=".48" stopColor="#ffffff" stopOpacity="0" /><stop offset="1" stopColor="#000000" stopOpacity={glass ? .03 : .1} />
      </linearGradient>
      <filter id={`${id}-shadow`} x="-50%" y="-50%" width="200%" height="200%"><feDropShadow dx="0" dy="1" stdDeviation="1" floodOpacity={d.shadow} /></filter>
    </defs>
    {raised && <g transform="translate(0 3)"><path d={path} fill={fill} fillOpacity={opacity} /><path d={path} fill="#000000" fillOpacity=".28" /></g>}
    <path d={path} fill={fill} fillOpacity={opacity} filter={d.shadow > 0 ? `url(#${id}-shadow)` : undefined} />
    <g clipPath={`url(#${id}-face)`}>
      {(glass || raised) && <rect width={w} height={h} fill={`url(#${id}-material)`} />}
      {glass && <path d={`M0 0H${w}L0 ${h * .6}Z`} fill="#ffffff" fillOpacity=".09" />}
      {d.keyMaterial === "paper" && <path d={Array.from({ length: Math.ceil((h - 3) / 4) }, (_, i) => `M0 ${3 + i * 4}L${w} ${2 + i * 4}`).join(" ")} stroke="#000000" strokeOpacity=".08" strokeWidth=".5" />}
    </g>
    {d.borderWidth > 0 && <path d={path} fill="none" stroke={hex(d.customBorderColor ?? d.accent)} strokeWidth={d.borderWidth} />}
  </g>;
}

export function SkinPreview({ content, name, compact = false }: { content: unknown; name: string; compact?: boolean }) {
  const [layout, setLayout] = useState<"full" | "nine">("full"); const prefix = useId();
  const parsed = designSchema.safeParse(content);
  if (!parsed.success) return <p className="rounded-[14px] bg-bad-soft px-4 py-3 text-[13.5px] text-bad" role="status">该皮肤设计格式暂不支持预览，请展开原始数据检查。</p>;
  const d = parsed.data; const bg = hex(d.background), ink = hex(d.keyForeground), accent = hex(d.accent);
  const patternID = `${prefix}-pattern`, gradientID = `${prefix}-gradient`, clipID = `${prefix}-clip`;
  const row = (labels: string[], y: number, start: number, width: number) => labels.map((label, i) => drawKey(label, start + i * (width + 5), y, width));
  function drawKey(label: string, x: number, y: number, width: number, action = false) {
    const text = action ? foreground(d.actionBackground) : ink;
    return <g key={`${x}-${y}`} transform={`translate(${x} ${y})`} data-key={label}>
      <KeySurface design={d} width={width} action={action} />
      {label === "删除" ? <Delete x={width / 2 - 10} y={11} width={20} height={20} color={text} aria-hidden="true" /> : label === "换行" ? <CornerDownLeft x={width / 2 - 10} y={11} width={20} height={20} color={text} aria-hidden="true" /> : label === "切换" ? <Globe x={width / 2 - 10} y={11} width={20} height={20} color={text} aria-hidden="true" /> : label === "大写" ? <ArrowUp x={width / 2 - 10} y={11} width={20} height={20} color={text} aria-hidden="true" /> : <text x={width / 2} y={27} textAnchor="middle" fill={text} fontSize={label.length > 1 ? 12 : 17}>{label}</text>}
    </g>;
  }
  const artwork = <svg className={compact ? "block h-full w-full" : "block h-auto w-full max-w-[440px] drop-shadow-xl"} viewBox="0 0 390 290" role="img" aria-label={`${name}，${layout === "full" ? "26 键" : "九键"}键盘效果`} fontFamily={d.monospaced ? "ui-monospace, monospace" : "system-ui, sans-serif"}>
      <title>{name}键盘示意预览</title><defs>
        <linearGradient id={gradientID} x1="0" y1="0" x2={d.gradientHorizontal ? "1" : "0"} y2={d.gradientHorizontal ? "0" : "1"}><stop stopColor={bg} /><stop offset="1" stopColor={hex(d.gradientEnd ?? d.background)} /></linearGradient>
        <pattern id={patternID} width={d.pattern === 2 ? 20 : 16} height={d.pattern === 2 ? 20 : 16} patternUnits="userSpaceOnUse">{d.pattern === 1 ? <circle cx="8.75" cy="8.75" r=".75" fill={accent} /> : <path d="M0 20V0H20" stroke={accent} strokeWidth=".5" fill="none" />}</pattern>
        <clipPath id={clipID}><rect width="390" height="290" rx="14" /></clipPath>
      </defs><g clipPath={`url(#${clipID})`}>
        <rect width="390" height="290" fill={`url(#${gradientID})`} />
        {d.photo && <><foreignObject width="390" height="290"><img src={`data:image/jpeg;base64,${d.photo}`} alt="" style={{ width: 390, height: 290, objectFit: "cover", objectPosition: `${(d.photoPosition ?? .5) * 100}% ${(d.photoPosition ?? .5) * 100}%` }} /></foreignObject><rect width="390" height="290" fill="#000000" opacity={d.photoShade ?? .25} /></>}
        {d.pattern > 0 && (d.pattern === 3
          ? <path d={Array.from({ length: 33 }, (_, i) => { const y = -100 + i * 24; return `M0 ${y}C136.5 ${y - 90} 253.5 ${y + 20} 390 ${y - 70}`; }).join(" ")} fill="none" stroke={accent} strokeWidth="2" opacity={d.patternOpacity ?? .15} />
          : <rect width="390" height="290" fill={`url(#${patternID})`} opacity={d.patternOpacity ?? .15} />)}
        <text x="14" y="22" fontSize="12" fill={ink}>shui shan</text><path d="M82 10V24" stroke={accent} />
        <text x="14" y="51" fontSize="19" fill={accent}>水杉</text><text x="83" y="51" fontSize="17" fill={ink}>水山</text><text x="148" y="51" fontSize="17" fill={ink}>水杉输入法</text>
        <path d="M12 64H378" stroke={ink} opacity=".12" />
        {layout === "full" ? <>{row([..."QWERTYUIOP"], 74, 7, 33.1)}{row([..."ASDFGHJKL"], 124, 25, 33.1)}{drawKey("大写", 7, 174, 42)}{row([..."ZXCVBNM"], 174, 54, 35)}{drawKey("删除", 334, 174, 49)}</> : <>{row(["符号", "ABC", "DEF"], 74, 7, 88)}{row(["GHI", "JKL", "MNO"], 124, 7, 88)}{row(["PQRS", "TUV", "WXYZ"], 174, 7, 88)}{drawKey("删除", 286, 74, 97)}{drawKey("分词", 286, 124, 97)}{drawKey("选词", 286, 174, 97, true)}</>}
        {drawKey("123", 7, 224, 46)}{drawKey("切换", 58, 224, 39)}{drawKey("空格", 102, 224, 183)}{drawKey("换行", 290, 224, 93, true)}
      </g>
    </svg>;
  if (compact) return <div className="h-full bg-panel-2" aria-hidden="true">{artwork}</div>;
  return <section className="my-4" aria-label={`${name}外观预览`}>
    <div className="flex flex-wrap items-center justify-between gap-3"><h3 className="m-0 text-[13px] font-bold text-ink">键盘预览</h3><Segmented size="sm" label="键盘布局" value={layout} onChange={setLayout} options={[{ value: "full", label: "26 键" }, { value: "nine", label: "九键" }]} /></div>
    <div className="mt-3 grid place-items-center rounded-xl bg-panel-2 p-5">{artwork}</div><p className="m-0 mt-2 text-xs leading-relaxed text-muted">根据皮肤参数渲染的示意效果；字体、纹理和材质细节可能与客户端略有差异。</p>
  </section>;
}

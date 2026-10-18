import { parse } from "smol-toml";
import { z } from "zod";
import palettes from "./candidate-palettes.json" with { type: "json" };

const colorKeys = ["surface", "border", "text", "number", "accent", "selected", "hover", "translation", "candidate_text", "preedit_text", "preedit_caret", "selected_text", "selected_number", "selected_translation", "selected_bar", "preedit_background", "preedit_divider"] as const;
const colorShape = Object.fromEntries(colorKeys.map(key => [key, z.string().max(128).optional()])) as Record<typeof colorKeys[number], z.ZodOptional<z.ZodString>>;
const paletteSchema = z.object({ ...colorShape, show_selected_bar: z.boolean().optional() });
const dimension = (maximum: number) => z.number().finite().min(0).max(maximum);
const designSchema = z.object({
  base: z.enum(["system", "shuishan", "light", "paper", "night", "ink", "fluent", "wechat", "graphite", "willow_green", "autumn_osmanthus", "microsoft"]),
  supports: z.object({ layouts: z.array(z.enum(["horizontal", "vertical"])).min(1), themes: z.array(z.enum(["dark", "light"])).min(1) }),
  candidate_window: z.object({
    min_width_dip: dimension(1000).default(0), corner_radius_dip: dimension(32).optional(), border_width_dip: dimension(4).optional(), item_corner_radius_dip: dimension(16).optional(),
    shadow: z.enum(["none", "soft", "strong"]).optional(), font_family: z.string().max(64).optional(), page_arrows: z.boolean().optional(),
    decoration: z.object({ image: z.string().optional(), top_inset_dip: dimension(500).default(0), width_dip: dimension(1000).default(0), align: z.enum(["left", "center", "right"]).default("right") }).optional(),
    background: z.object({ image: z.string(), fit: z.enum(["cover", "contain", "stretch"]).default("cover"), opacity: dimension(1).default(1) }).optional(),
  }),
  candidate: z.object({ dark: paletteSchema.optional(), light: paletteSchema.optional() }).optional(),
});
export type CandidateDesign = z.infer<typeof designSchema>;
export type CandidateMode = "light" | "dark";
export type CandidateLayout = "horizontal" | "vertical";

export function parseCandidateDesign(manifest: string): CandidateDesign | null {
  if (new TextEncoder().encode(manifest).length > 65536) return null;
  try { return designSchema.parse(parse(manifest)); } catch { return null; }
}

// 客户端只读取包内资源，不能把清单中的外部地址或越界路径当成图片。
export function candidateImagePath(path: string | undefined): string | null {
  return path && path.length <= 256 && !/[\\:?#]/.test(path) && [...path].every(char => char.charCodeAt(0) > 31 && char.charCodeAt(0) !== 127) && path.split("/").every(part => part !== "" && part !== "." && part !== "..") && /\.(png|jpe?g)$/i.test(path) ? path : null;
}

// 对齐客户端 theme::normalized_color；不把任意 CSS 值当作可用颜色。
export function candidateColor(value: unknown): string | null {
  if (typeof value !== "string") return null;
  const color = value.trim();
  if (color.toLowerCase() === "transparent") return "#00000000";
  if (/^#[\da-f]{3}$/i.test(color)) return `#${[...color.slice(1)].map(c => c + c).join("")}`;
  if (/^#(?:[\da-f]{6}|[\da-f]{8})$/i.test(color)) return color;
  const match = /^(rgb|rgba)\(([^)]*)\)$/i.exec(color);
  if (!match) return null;
  const parts = match[2].split(",").map(part => part.trim());
  const alpha = match[1].toLowerCase() === "rgba";
  if (parts.length !== (alpha ? 4 : 3)) return null;
  const channels = parts.slice(0, 3).map(part => /^\+?\d+$/.test(part) ? Number(part) : NaN);
  if (channels.some(c => !Number.isFinite(c) || c > 255)) return null;
  const hex = (c: number) => c.toString(16).padStart(2, "0");
  const rgb = `#${channels.map(hex).join("")}`;
  if (!alpha) return rgb;
  if (!/^[+-]?(\d+(\.\d*)?|\.\d+)(e[+-]?\d+)?$/i.test(parts[3])) return null;
  const opacity = Number(parts[3]);
  return opacity >= 0 && opacity <= 1 ? `${rgb}${hex(Math.round(opacity * 255))}` : null;
}

type Palette = Record<string, string | boolean | null>;
type BasePalette = { light: Palette; dark: Palette; radius?: number };
const bases: Record<string, BasePalette> = palettes;

// 基础配色来自客户端 theme-catalog.json 和 catalog/windows_looks.rs，再叠加清单当前明暗模式的字段。
export function candidatePalette(design: CandidateDesign, mode: CandidateMode): { colors: Palette; radius: number } {
  const base = bases[design.base];
  const colors: Palette = mode === "dark"
    ? { surface: "#202020", border: "#9b9b9b2e", text: "#e9e8e8", number: "#e9e8e89d", accent: "#6b69d6", selected: "#3e3e3eb9", hover: "#414141" }
    : { surface: "#ffffff", border: "#0000001f", text: "#1a1a1a", number: "#1a1a1a8c", accent: "#6b69d6", selected: "#e8e8e8", hover: "#ececec" };
  if (base) Object.assign(colors, base[mode]);
  const custom = design.candidate?.[mode];
  for (const key of colorKeys) {
    const color = candidateColor(custom?.[key]);
    if (color) colors[key] = color;
  }
  if (typeof custom?.show_selected_bar === "boolean") colors.show_selected_bar = custom.show_selected_bar;
  if (base && base.radius === undefined) {
    if (!candidateColor(custom?.selected)) colors.selected = `${String(colors.accent).slice(0, 7)}24`;
    if (!candidateColor(custom?.selected_text)) colors.selected_text = colors.accent;
    if (!candidateColor(custom?.selected_number)) colors.selected_number = colors.number;
  }
  return { colors, radius: design.candidate_window.corner_radius_dip ?? base?.radius ?? 6 };
}

import { useEffect, useRef, useState } from "react";
import type { CSSProperties } from "react";
import { useAPIImage } from "./hooks/use-api-image";
import { keys } from "./api/keys";
import { candidateImagePath, candidatePalette } from "./candidate-design";
import type { CandidateDesign, CandidateLayout, CandidateMode } from "./candidate-design";
import { Segmented } from "./ui/segmented";
import "./candidate-preview.css";

type ImageFile = { file: File; path: string };
type ImageSource = { id?: string; revision?: string | null; files?: ImageFile[] };

function useCandidateImage(path: string | undefined, source: ImageSource) {
  const safe = candidateImagePath(path);
  const selected = source.files?.find(entry => entry.path === safe)?.file;
  const [local, setLocal] = useState<{ file: File; data?: string; error?: boolean }>();
  useEffect(() => {
    if (!selected) return;
    if (selected.size > 1024 * 1024) { setLocal({ file: selected, error: true }); return; }
    const reader = new FileReader();
    reader.onload = () => setLocal({ file: selected, data: String(reader.result) });
    reader.onerror = () => setLocal({ file: selected, error: true });
    reader.readAsDataURL(selected);
    return () => { reader.onload = null; reader.onerror = null; if (reader.readyState === FileReader.LOADING) reader.abort(); };
  }, [selected]);
  const remote = useAPIImage({
    queryKey: keys.page("community", "candidate-image", source.id, source.revision, safe),
    path: `/api/candidate-skins/${encodeURIComponent(source.id ?? "")}/image?path=${encodeURIComponent(safe ?? "")}`,
    enabled: Boolean(source.id && safe && !source.files?.length),
  });
  if (!path) return {};
  if (!safe) return { error: true };
  if (source.files?.length || !source.id) return selected && local?.file === selected ? local : { error: !selected };
  return { data: remote.data, error: remote.isError };
}

function Asset({ src, label, style, className }: { src: string; label: string; style?: CSSProperties; className: string }) {
  const [failed, setFailed] = useState(false);
  return failed ? <span role="status">{label}无法显示</span> : <img src={src} alt={label} className={className} style={style} onError={() => setFailed(true)} />;
}

const samples = ["你们", "你", "尼", "妮", "泥", "逆"];

export function CandidateArtwork({ design, name, source, compact = false }: { design: CandidateDesign; name: string; source: ImageSource; compact?: boolean }) {
  const [layout, setLayout] = useState<CandidateLayout>("horizontal");
  const [mode, setMode] = useState<CandidateMode>("light");
  const orientation = design.supports.layouts.includes(layout) ? layout : design.supports.layouts[0];
  const appearance = design.supports.themes.includes(mode) ? mode : design.supports.themes[0];
  const { colors, radius } = candidatePalette(design, appearance);
  const window = design.candidate_window;
  const decoration = window.decoration;
  const decorated = Boolean(decoration?.image && decoration.top_inset_dip > 0 && decoration.width_dip > 0);
  const character = useCandidateImage(decorated ? decoration?.image : undefined, source);
  const background = useCandidateImage(window.background?.image, source);
  const stage = useRef<HTMLDivElement>(null), art = useRef<HTMLDivElement>(null);
  const [scale, setScale] = useState(1);
  useEffect(() => {
    if (!compact || !stage.current || !art.current) return;
    const outer = stage.current, inner = art.current;
    const update = () => setScale(Math.min(1, (outer.clientWidth - 16) / Math.max(1, inner.offsetWidth), (outer.clientHeight - 16) / Math.max(1, inner.offsetHeight)));
    const observer = new ResizeObserver(update);
    observer.observe(outer); observer.observe(inner); update();
    return () => observer.disconnect();
  }, [compact]);
  const variables = Object.fromEntries(Object.entries(colors).filter(([, value]) => typeof value === "string").map(([key, value]) => [`--candidate-${key.replaceAll("_", "-")}`, value]));
  const style = {
    ...variables, "--candidate-radius": `${radius}px`, "--candidate-item-radius": `${window.item_corner_radius_dip ?? 4}px`,
    "--candidate-border-width": `${window.border_width_dip ?? 1.5}px`, "--candidate-bar": colors.show_selected_bar === false ? "none" : "block",
    "--candidate-shadow": window.shadow === "none" ? "none" : window.shadow === "strong" ? "5px 5px 16px #00000066" : appearance === "dark" ? "5px 5px 10px #00000080" : "4px 4px 12px #0000001f",
    fontFamily: window.font_family ? `${window.font_family}, system-ui, sans-serif` : '"Segoe UI", "Noto Sans SC", "Microsoft YaHei", sans-serif',
    minWidth: compact ? window.min_width_dip : `min(${window.min_width_dip}px, 100%)`, paddingTop: decorated ? decoration?.top_inset_dip : 0,
    ...(compact ? { position: "absolute", left: "50%", top: "50%", transform: `translate(-50%, -50%) scale(${scale})` } : {}),
  } as CSSProperties;
  return <section className={compact ? "h-full" : "my-4"} aria-label={`${name}候选窗外观预览`}>
    {!compact && <div className="mb-3 flex flex-wrap items-center justify-between gap-2">
      <h3 className="m-0 text-[13px] font-bold text-ink">候选窗预览</h3>
      <Segmented size="sm" label="候选窗布局" value={orientation} onChange={setLayout} options={design.supports.layouts.map(value => ({ value, label: value === "horizontal" ? "横向" : "纵向" }))} />
      <Segmented size="sm" label="候选窗明暗" value={appearance} onChange={setMode} options={design.supports.themes.map(value => ({ value, label: value === "light" ? "浅色" : "深色" }))} />
    </div>}
    <div ref={stage} className={`candidate-stage${compact ? " candidate-stage-compact" : ""}`}>
      <div ref={art} className="candidate-art" data-layout={orientation} data-mode={appearance} role="img" aria-label={`${name}候选窗效果`} style={style}>
        {decorated && character.data && <Asset key={character.data} src={character.data} label="候选窗装饰" className="candidate-decoration" style={{ width: decoration?.width_dip, height: decoration?.top_inset_dip, left: decoration?.align === "left" ? 0 : decoration?.align === "center" ? "50%" : undefined, right: decoration?.align === "right" ? 0 : undefined, transform: decoration?.align === "center" ? "translateX(-50%)" : undefined, objectPosition: `${decoration?.align} bottom` }} />}
        <div className="candidate-surface">
          {background.data && <Asset key={background.data} src={background.data} label="候选窗背景" className="candidate-background" style={{ objectFit: window.background?.fit === "stretch" ? "fill" : window.background?.fit, opacity: window.background?.opacity }} />}
          <div className="candidate-preedit">ni'mf<span className="candidate-caret" /></div>
          <div className="candidate-rows">{samples.map((text, index) => <div key={text} className={`candidate-row${index === 0 ? " candidate-selected" : ""}`}>
            <span className="candidate-number">{index + 1}</span><span className="candidate-word">{text}</span>
          </div>)}</div>
          {window.page_arrows && <div className="candidate-pages">‹　1 / 2　›</div>}
        </div>
      </div>
    </div>
    {(character.error || background.error) && <p role="status" className="m-0 mt-2 text-xs text-bad">背景或装饰图片缺失，无法完整预览，请检查包内路径和文件。</p>}
    {!compact && <p className="m-0 mt-2 text-xs leading-relaxed text-muted">按清单和包内图片渲染，使用固定候选词。系统字体与窗口特效随客户端平台变化。</p>}
  </section>;
}

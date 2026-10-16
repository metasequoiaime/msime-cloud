import { formatNumber } from "../../utils/number";
import { useCallback, useContext, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import { formatDateTime } from "../../utils/time";
import { useDebounced } from "../../hooks/use-debounced";
import { Pause, Play, Search } from "lucide-react";
import { ToggleGroup } from "radix-ui";
import { APICredentialsContext, APIError, errorMessage } from "../../api/client";
import type { LogLevel, LogLine, StreamEvent } from "../../api/logs";
import { compareTs, logLevels, openLogStream, streamGapSchema, streamLinesSchema, streamPodsSchema } from "../../api/logs";
import { PageIntro } from "../../shell/page-intro";
import { usePermissions } from "../../shell/permissions";
import { useShell } from "../../shell/shell-data";
import { Button } from "../../ui/button";
import { cn } from "../../ui/cn";
import { Dot } from "../../ui/pill";
import type { SegmentOption } from "../../ui/segmented";
import { Segmented } from "../../ui/segmented";
import { Empty, ErrorState, NotConfigured, SkeletonRows } from "../../ui/states";

// 内存里最多保留的行数，超出时丢弃最旧的。
const maxRows = 2000;
// 首次连接时回填的行数。
const backfill = 500;
// 离底部不超过这个距离（像素）时视为停在底部，新行到达会自动滚动。
const stickThreshold = 24;

type Row = { kind: "line"; id: number; line: LogLine } | { kind: "gap"; id: number; ts: string; from: string; to: string };
type Status = "connecting" | "live" | "reconnecting" | "failed";

function rowTs(row: Row): string {
  return row.kind === "line" ? row.line.ts : row.ts;
}

// mergeRows 把新到的行按时间戳插入（晚到的行可能早于已显示的最后一行），再截到 maxRows。
function mergeRows(previous: Row[], incoming: Row[]): Row[] {
  if (incoming.length === 0) return previous;
  const rows = previous.slice();
  for (const row of incoming) {
    let index = rows.length;
    while (index > 0 && compareTs(rowTs(rows[index - 1]), rowTs(row)) > 0) index--;
    rows.splice(index, 0, row);
  }
  return rows.length > maxRows ? rows.slice(rows.length - maxRows) : rows;
}

// 副本按名称排序后循环使用这几种颜色，日志行的副本标记和顶部按钮上的色点一致。badge 是标记的底色和字色，dot 是按钮上的色点。只用不随季节变化的色相（图表色和信息蓝，外加一个紫色），避免秋季橙色主题色和橙色图表色撞色。
const podTones = [
  { badge: "bg-chart-1/15 text-chart-1", dot: "bg-chart-1" },
  { badge: "bg-info-soft text-info", dot: "bg-info" },
  { badge: "bg-chart-3/15 text-chart-3", dot: "bg-chart-3" },
  { badge: "bg-[#8E5BB5]/15 text-[#8E5BB5]", dot: "bg-[#8E5BB5]" },
  { badge: "bg-panel-2 text-body", dot: "bg-muted" },
] as const;
type PodTone = (typeof podTones)[number];

// allPodsKey 是「全部副本」按钮的键：ToggleGroup 用空字符串表示未选中，而合法的副本名不会以下划线开头。
const allPodsKey = "_all";

// shortPod 取副本名的最后两段（ReplicaSet 哈希和副本 ID），完整名称放在 title 里。
function shortPod(pod: string): string {
  const parts = pod.split("-");
  return parts.length > 2 ? parts.slice(-2).join("-") : pod;
}

const levelText: Record<string, string> = { ERROR: "text-bad", WARN: "text-warn", INFO: "text-info", DEBUG: "text-muted" };

type LevelFilter = "all" | LogLevel;
const levelOptions: readonly SegmentOption<LevelFilter>[] = [
  { value: "all", label: "全部" },
  ...logLevels.map(level => ({ value: level, label: level === "INFO" ? "INFO 及以上" : level === "WARN" ? "WARN 及以上" : "ERROR" })),
];

const pageCodeMessages: Record<string, string> = { invalid_level: "日志级别无效，请刷新后重试。" };

function streamError(error: unknown): string {
  if (error instanceof APIError && Object.hasOwn(pageCodeMessages, error.code)) return pageCodeMessages[error.code];
  return errorMessage(error);
}

function waitOrAbort(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise(resolve => {
    const timer = window.setTimeout(resolve, ms);
    signal.addEventListener("abort", () => { window.clearTimeout(timer); resolve(); }, { once: true });
  });
}

// PodChips 是按副本筛选的单选按钮，样式同 FilterChips，每个副本前加上它的色点。
function PodChips({ pods, value, onChange, tone }: { pods: readonly string[]; value: string; onChange: (pod: string) => void; tone: (pod: string) => PodTone }) {
  return <ToggleGroup.Root type="single" value={value || allPodsKey} aria-label="按副本筛选" onValueChange={next => { if (next) onChange(next === allPodsKey ? "" : next); }}
    className="flex flex-wrap items-center gap-1.5">
    {[allPodsKey, ...pods].map(key => <ToggleGroup.Item key={key} value={key} title={key === allPodsKey ? undefined : key}
      className="inline-flex h-[30px] items-center gap-1.5 rounded-full px-3 whitespace-nowrap text-[13px] text-body transition hover:bg-panel-2 data-[state=on]:bg-btn data-[state=on]:font-semibold data-[state=on]:text-btn-fg">
      {key !== allPodsKey && <span aria-hidden="true" className={cn("h-2 w-2 shrink-0 rounded-full", tone(key).dot)} />}
      {key === allPodsKey ? "全部副本" : shortPod(key)}
    </ToggleGroup.Item>)}
  </ToggleGroup.Root>;
}

function LogRow({ row, tone }: { row: Row; tone: (pod: string) => PodTone }) {
  if (row.kind === "gap") {
    return <div className="my-1 px-3 py-1 text-center text-[12px] text-warn" role="note">日志超出单个连接的读取上限，部分日志未显示</div>;
  }
  const { line } = row;
  return <div className={cn("flex items-start gap-2 px-3 py-[3px] hover:bg-panel-2", line.level === "ERROR" && "bg-bad-soft")}>
    <time dateTime={line.time} title={`${line.time}${line.stream ? ` · ${line.stream}` : ""}`} className="shrink-0 text-muted tabular-nums">{formatDateTime(line.time, "HH:mm:ss.SSS")}</time>
    <span title={line.pod} className={cn("inline-flex h-[18px] shrink-0 items-center rounded-[5px] px-1.5 text-[11px]", tone(line.pod).badge)}>{shortPod(line.pod) || "?"}</span>
    <span className={cn("w-[44px] shrink-0 font-semibold", levelText[line.level] ?? "text-muted")}>{line.level || "—"}</span>
    <span className="min-w-0 flex-1 break-all whitespace-pre-wrap text-body">{line.message}</span>
  </div>;
}

export default function LogsPage() {
  const shell = useShell();
  const permissions = usePermissions();
  const credentials = useContext(APICredentialsContext);
  if (!credentials) throw new Error("AuthProvider required");
  const configured = shell.data?.features?.logs ?? false;
  if (!permissions.loaded) return <><PageIntro page="logs" /><SkeletonRows rows={8} className="rounded-[18px] bg-panel ring-1 ring-hair" /></>;
  if (!configured) return <><PageIntro page="logs" /><NotConfigured>服务端未配置日志来源（admin.logs.loki_url），配置集群 Loki 的地址后这里会显示后端各副本的实时日志。</NotConfigured></>;
  if (!permissions.can("view_logs")) return <><PageIntro page="logs" /><Empty title="当前角色没有查看服务日志的权限（view_logs）" /></>;
  return <LiveLogs token={credentials.token} onUnauthorized={credentials.onUnauthorized} />;
}

function LiveLogs({ token, onUnauthorized }: { token: string; onUnauthorized: () => void }) {
  const [pod, setPod] = useState("");
  const [level, setLevel] = useState<LevelFilter>("all");
  const [typed, setTyped] = useState("");
  const q = useDebounced(typed.trim(), 400);
  const [rows, setRows] = useState<Row[]>([]);
  const [pods, setPods] = useState<string[]>([]);
  const [status, setStatus] = useState<Status>("connecting");
  const [failure, setFailure] = useState<unknown>(null);
  const [lokiDown, setLokiDown] = useState(false);
  const [paused, setPaused] = useState(false);
  const [pending, setPending] = useState(0);
  const [stuck, setStuck] = useState(true);
  const [restart, setRestart] = useState(0);
  const pausedRef = useRef(false);
  const bufferRef = useRef<Row[]>([]);
  const idRef = useRef(0);
  const scroller = useRef<HTMLDivElement>(null);
  const stuckRef = useRef(true);

  const append = useCallback((incoming: Row[]) => {
    if (pausedRef.current) {
      bufferRef.current = mergeRows(bufferRef.current, incoming);
      setPending(bufferRef.current.length);
      return;
    }
    setRows(previous => mergeRows(previous, incoming));
  }, []);

  // 筛选条件变化时清空并重新连接；连接断开后带游标重连，从断开处继续。
  useEffect(() => {
    void restart;
    const controller = new AbortController();
    let cursor = "";
    setRows([]);
    bufferRef.current = [];
    setPending(0);
    setFailure(null);
    setLokiDown(false);
    const handle = (event: StreamEvent) => {
      if (event.id) cursor = event.id;
      setStatus("live");
      switch (event.event) {
        case "lines": {
          const { lines } = streamLinesSchema.parse(JSON.parse(event.data));
          setLokiDown(false);
          append(lines.map(line => ({ kind: "line", id: ++idRef.current, line })));
          break;
        }
        case "pods":
          setPods(streamPodsSchema.parse(JSON.parse(event.data)).pods);
          break;
        case "gap": {
          const gap = streamGapSchema.parse(JSON.parse(event.data));
          append([{ kind: "gap", id: ++idRef.current, ts: gap.to, from: gap.from, to: gap.to }]);
          break;
        }
        case "error":
          setLokiDown(true);
          break;
      }
    };
    const run = async () => {
      let attempt = 0;
      while (!controller.signal.aborted) {
        const query = new URLSearchParams();
        if (pod) query.set("pod", pod);
        if (level !== "all") query.set("level", level);
        if (q) query.set("q", q);
        if (cursor) query.set("cursor", cursor);
        else query.set("backfill", String(backfill));
        setStatus(attempt === 0 ? "connecting" : "reconnecting");
        try {
          await openLogStream(query, { token, signal: controller.signal, onEvent: handle });
          attempt = 0;
          setFailure(null);
        } catch (error) {
          if (controller.signal.aborted) return;
          if (error instanceof APIError) {
            if (error.status === 401) {
              onUnauthorized();
              return;
            }
            // 权限、参数和功能开关的错误重试也不会好，停下来显示原因。
            if (error.status === 400 || error.status === 403 || error.status === 404) {
              setFailure(error);
              setStatus("failed");
              return;
            }
          }
          setFailure(error);
          attempt++;
        }
        await waitOrAbort(attempt === 0 ? 500 : Math.min(30_000, 1000 * 2 ** attempt), controller.signal);
      }
    };
    void run();
    return () => controller.abort();
  }, [pod, level, q, token, onUnauthorized, append, restart]);

  const togglePause = () => {
    const next = !paused;
    pausedRef.current = next;
    setPaused(next);
    if (!next) {
      const buffered = bufferRef.current;
      bufferRef.current = [];
      setPending(0);
      setRows(previous => mergeRows(previous, buffered));
    }
  };

  const onScroll = () => {
    const el = scroller.current;
    if (!el) return;
    const atBottom = el.scrollHeight - el.scrollTop - el.clientHeight <= stickThreshold;
    if (atBottom !== stuckRef.current) {
      stuckRef.current = atBottom;
      setStuck(atBottom);
    }
  };
  const toBottom = () => {
    stuckRef.current = true;
    setStuck(true);
    const el = scroller.current;
    if (el) el.scrollTop = el.scrollHeight;
  };
  useLayoutEffect(() => {
    void rows;
    const el = scroller.current;
    if (el && stuckRef.current) el.scrollTop = el.scrollHeight;
  }, [rows]);

  // 按钮列出窗口内有日志的副本，也包括已经显示的行里出现、但窗口内已没有新日志的副本。
  const allPods = useMemo(() => {
    const set = new Set(pods);
    for (const row of rows) if (row.kind === "line" && row.line.pod) set.add(row.line.pod);
    if (pod) set.add(pod);
    return [...set].sort();
  }, [pods, rows, pod]);
  const tone = useCallback((name: string): PodTone => {
    const index = allPods.indexOf(name);
    return index < 0 ? podTones[podTones.length - 1] : podTones[index % podTones.length];
  }, [allPods]);
  const lineCount = rows.filter(row => row.kind === "line").length;

  const statusView = paused
    ? { tone: "warn" as const, text: pending > 0 ? `已暂停，缓冲 ${pending} 行` : "已暂停" }
    : status === "live" && !lokiDown ? { tone: "ok" as const, text: "实时" }
      : status === "failed" ? { tone: "bad" as const, text: "已停止" }
        : lokiDown ? { tone: "warn" as const, text: "日志服务暂时不可用，正在重试" }
          : { tone: "mute" as const, text: status === "reconnecting" ? "正在重新连接…" : "正在连接…" };

  return <>
    <PageIntro page="logs">
      <span className="inline-flex items-center gap-1.5 text-xs text-muted" role="status"><Dot tone={statusView.tone} />{statusView.text}</span>
    </PageIntro>
    <div className="mb-3.5 flex flex-wrap items-center gap-3 rounded-[18px] bg-panel px-3.5 py-3 ring-1 ring-hair">
      <div className="flex min-w-0 grow basis-[320px] flex-wrap items-center gap-1.5">
        <PodChips pods={allPods} value={pod} onChange={setPod} tone={tone} />
      </div>
      <Segmented label="级别" size="sm" value={level} options={levelOptions} onChange={setLevel} />
      <label className="relative flex h-[30px] w-full min-[820px]:w-[220px]">
        <span className="sr-only">搜索日志内容</span>
        <Search size={15} aria-hidden="true" className="pointer-events-none absolute top-1/2 left-2.5 -translate-y-1/2 text-muted" />
        <input type="search" value={typed} maxLength={200} onChange={event => setTyped(event.target.value)} placeholder="包含的文字（区分大小写）"
          className="h-[30px] w-full rounded-lg bg-panel-2 pr-2.5 pl-8 text-[13px] text-ink outline-none placeholder:text-muted focus:ring-[1.5px] focus:ring-accent" />
      </label>
      <Button size="sm" onClick={togglePause} aria-pressed={paused}>{paused ? <><Play size={14} aria-hidden="true" />继续</> : <><Pause size={14} aria-hidden="true" />暂停</>}</Button>
    </div>
    {failure != null && <ErrorState error={new Error(streamError(failure))} className="mb-3.5" onRetry={status === "failed" ? () => setRestart(value => value + 1) : undefined} />}
    <div className="relative">
      <div ref={scroller} onScroll={onScroll} role="log" aria-live="off" aria-label="服务日志"
        className="h-[calc(100vh-280px)] min-h-[320px] overflow-auto rounded-[18px] bg-panel py-2 font-mono text-[12.5px] leading-[1.6] ring-1 ring-hair">
        {rows.length === 0
          ? status === "live" || status === "failed" ? <Empty title={q || level !== "all" || pod ? "没有符合条件的日志" : "最近 15 分钟没有日志"} /> : <SkeletonRows rows={8} />
          : rows.map(row => <LogRow key={row.id} row={row} tone={tone} />)}
      </div>
      {!stuck && rows.length > 0 && <Button size="sm" variant="primary" className="absolute right-4 bottom-4 shadow-[var(--shadow)]" onClick={toBottom}>回到最新</Button>}
    </div>
    <p className="m-0 mt-2 text-xs text-muted tabular-nums">显示 {formatNumber(lineCount)} 行，最多保留最近 {formatNumber(maxRows)} 行 · 级别为 WARN 或 ERROR 时，无法识别级别的行（如 panic）也会显示</p>
  </>;
}

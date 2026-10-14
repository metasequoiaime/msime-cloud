// Component harness for tests/csp_smoke.py: renders every shared ui piece with fixed test fixtures so the smoke test can exercise them under the production CSP.
import "../../src/zod-config";
import { StrictMode, useState } from "react";
import { createRoot } from "react-dom/client";
import { Button } from "../../src/ui/button";
import { Card, CardHeader, Banner } from "../../src/ui/card";
import { AreaChart, ChartLegend, Sparkline } from "../../src/ui/charts";
import { ConfirmProvider, useConfirm } from "../../src/ui/confirm";
import { CellText, DataTable } from "../../src/ui/data-table";
import type { Column } from "../../src/ui/data-table";
import { DetailDrawer, DrawerComposer } from "../../src/ui/drawer";
import { FilterChips } from "../../src/ui/filter-chips";
import { KeyboardPreview } from "../../src/ui/keyboard-preview";
import { Pill } from "../../src/ui/pill";
import { ProgressBar } from "../../src/ui/progress";
import { Segmented } from "../../src/ui/segmented";
import { StatGrid, StatTile } from "../../src/ui/stat-tile";
import { Switch } from "../../src/ui/switch";
import { ToastProvider, useToast } from "../../src/ui/toast";
import { useDebounced } from "../../src/hooks/use-debounced";
import "./harness.css";

type Row = { id: string; title: string; state: "open" | "closed" };
const rows: Row[] = [{ id: "a", title: "第一行", state: "open" }, { id: "b", title: "第二行", state: "open" }, { id: "c", title: "第三行", state: "closed" }];
const columns: Column<Row>[] = [
  { id: "title", header: "标题", width: "minmax(200px,2fr)", cell: row => <CellText title={row.title} sub={row.id} /> },
  { id: "state", header: "状态", width: "90px", cell: row => <Pill tone={row.state === "open" ? "warn" : "mute"}>{row.state}</Pill> },
];
const series = Array.from({ length: 30 }, (_, index) => ({ day: `09-${String(index + 1).padStart(2, "0")}`, a: 100 + index * 3, b: 50 + (index % 7) * 9 }));
const skin = { background: 0xf5faf6, keyBackground: 0xffffff, keyForeground: 0x0f2419, accent: 0x1e8e4e, actionBackground: 0x1e8e4e, cornerRadius: 8, borderWidth: 1, shadow: 0.1, pattern: 1, monospaced: false };

function DebounceHarness() {
  const [value, setValue] = useState("初始值");
  const [delay, setDelay] = useState(200);
  const debounced = useDebounced(value, delay);
  return <Card>
    <CardHeader title="防抖" />
    <input aria-label="防抖输入" value={value} onChange={event => setValue(event.target.value)} />
    <select aria-label="防抖延迟" value={delay} onChange={event => setDelay(Number(event.target.value))}>
      <option value={200}>200ms</option><option value={400}>400ms</option>
    </select>
    <output data-testid="debounced-value">{debounced}</output>
  </Card>;
}

function Harness() {
  const confirm = useConfirm();
  const toast = useToast();
  const [filter, setFilter] = useState<"all" | "open">("all");
  const [mode, setMode] = useState<"a" | "b">("a");
  const [on, setOn] = useState(false);
  const [drawer, setDrawer] = useState(false);
  const [reply, setReply] = useState("");
  const [committed, setCommitted] = useState("none");
  return <main className="grid gap-4 p-6">
    <DebounceHarness />
    <StatGrid kpi><StatTile size="kpi" label="KPI" value="1,234" delta="+12%" sub="较上月" /><StatTile label="Stat" value={7} delta="-3" /></StatGrid>
    <Card><CardHeader title="图表" actions={<ChartLegend series={[{ key: "a", label: "A" }, { key: "b", label: "B" }]} />} />
      <AreaChart data={series} xKey="day" series={[{ key: "a", label: "A" }, { key: "b", label: "B" }]} ariaLabel="面积图" />
      <AreaChart data={series} xKey="day" filled={false} series={[{ key: "a", label: "A" }]} ariaLabel="折线图" />
      <Sparkline data={series.map(point => point.a)} ariaLabel="迷你图" />
      <ProgressBar value={40} label="进度" />
    </Card>
    <div className="flex flex-wrap items-center gap-2">
      <Segmented label="模式" value={mode} onChange={setMode} options={[{ value: "a", label: "甲" }, { value: "b", label: "乙" }]} />
      <Switch label="开关" checked={on} onCheckedChange={setOn} />
      <Button variant="primary" onClick={() => setDrawer(true)}>打开抽屉</Button>
      <Button onClick={() => { setCommitted("waiting"); toast({ text: "已合并", delayCommit: async () => setCommitted("yes"), undo: () => setCommitted("undone") }); }}>延迟提交</Button>
      <Button onClick={() => toast({ text: "已驳回", delayCommit: () => Promise.reject(new Error("提交失败")), undo: () => undefined })}>延迟失败</Button>
      <Button onClick={() => toast("这是一条提示")}>普通提示</Button>
      <span data-testid="committed">{committed}</span>
    </div>
    <Banner>说明横幅</Banner>
    <DataTable ariaLabel="测试表格" data={filter === "all" ? rows : rows.filter(row => row.state === "open")} columns={columns} getRowId={row => row.id} selectable searchText={row => row.title}
      toolbar={<FilterChips label="状态" value={filter} onChange={setFilter} options={[{ key: "all", label: "全部", count: 3 }, { key: "open", label: "待处理", count: 2 }]} />}
      onRowClick={() => setDrawer(true)}
      batchActions={[{ label: "批量驳回", onClick: async selected => {
        const reason = await confirm({ title: `驳回 ${selected.length} 项？`, description: "测试确认框", okLabel: "驳回", reasons: ["原因甲", "原因乙"] });
        if (reason === null) return false;
        toast({ text: `已驳回：${reason}`, undo: () => undefined });
      } }, { label: "批量失败", onClick: () => Promise.reject(new Error("测试失败")) }]} />
    <DetailDrawer open={drawer} onClose={() => setDrawer(false)} title="详情" sub="副标题" pills={[{ text: "待审核", tone: "warn" }]}
      fields={[{ label: "ID", value: "abc", mono: true }, { label: "作者", value: "someone" }]}
      sections={[{ title: "记录", items: [{ text: "一条记录", meta: "刚刚" }] }, { title: "空", items: [] }]}
      actions={[{ label: "通过", variant: "primary", onClick: () => toast("已通过") }]}
      composer={<DrawerComposer value={reply} onChange={setReply} onSend={() => setReply("")} templates={[{ name: "模板", text: "模板内容" }]} />}>
      <KeyboardPreview design={skin} name="测试皮肤" />
    </DetailDrawer>
  </main>;
}

const root = document.getElementById("root");
if (!root) throw new Error("Missing harness root");
createRoot(root).render(<StrictMode><ToastProvider><ConfirmProvider><Harness /></ConfirmProvider></ToastProvider></StrictMode>);

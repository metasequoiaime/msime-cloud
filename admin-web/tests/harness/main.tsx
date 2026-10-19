// Component harness for tests/csp_smoke.py: renders every shared ui piece with fixed test fixtures so the smoke test can exercise them under the production CSP.
import "../../src/zod-config";
import { StrictMode, useRef, useState } from "react";
import { createRoot } from "react-dom/client";
import { QueryClient, QueryClientProvider, useMutation } from "@tanstack/react-query";
import { z } from "zod";
import { AuthProvider, useAuth } from "../../src/auth";
import { APICredentialsContext, APIError, useAPI } from "../../src/api/client";
import { Button } from "../../src/ui/button";
import { Card, CardHeader, Banner } from "../../src/ui/card";
import { AreaChart, ChartLegend, Sparkline } from "../../src/ui/charts";
import { ConfirmProvider, useConfirm } from "../../src/ui/confirm";
import { CountBar } from "../../src/ui/count-bar";
import { CellText, DataTable } from "../../src/ui/data-table";
import type { Column } from "../../src/ui/data-table";
import { DetailDrawer, DrawerComposer } from "../../src/ui/drawer";
import { FilterChips } from "../../src/ui/filter-chips";
import { KeyboardPreview } from "../../src/ui/keyboard-preview";
import { ModalFrame, ModalHeading } from "../../src/ui/modal";
import { useOverlayOpen } from "../../src/ui/overlay";
import { Pill } from "../../src/ui/pill";
import { ProgressBar } from "../../src/ui/progress";
import { Segmented } from "../../src/ui/segmented";
import { StatGrid, StatTile } from "../../src/ui/stat-tile";
import { Switch } from "../../src/ui/switch";
import { ToastProvider, useToast } from "../../src/ui/toast";
import { useAPIImage } from "../../src/hooks/use-api-image";
import { keys } from "../../src/api/keys";
import { useDebounced } from "../../src/hooks/use-debounced";
import { formatBytes as dictionaryBytes } from "../../src/pages/dictionary";
import { formatBytes as communityBytes } from "../../src/pages/community/format";
import { formatSize as releaseBytes } from "../../src/pages/release/shared";
import "./harness.css";

type Row = { id: string; title: string; state: "open" | "closed" };
const rows: Row[] = [{ id: "a", title: "第一行", state: "open" }, { id: "b", title: "第二行", state: "open" }, { id: "c", title: "第三行", state: "closed" }];
const columns: Column<Row>[] = [
  { id: "title", header: "标题", width: "minmax(200px,2fr)", cell: row => <CellText title={row.title} sub={row.id} /> },
  { id: "state", header: "状态", width: "90px", cell: row => <Pill tone={row.state === "open" ? "warn" : "mute"}>{row.state}</Pill> },
];
const series = Array.from({ length: 30 }, (_, index) => ({ day: `09-${String(index + 1).padStart(2, "0")}`, a: 1100 + index * 30, b: 50 + (index % 7) * 9 }));
const skin = { background: 0xf5faf6, keyBackground: 0xffffff, keyForeground: 0x0f2419, accent: 0x1e8e4e, actionBackground: 0x1e8e4e, cornerRadius: 8, borderWidth: 1, shadow: 0.1, pattern: 1, monospaced: false };

const authClient = new QueryClient();
const imageClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
const imageCredentials = { token: new URLSearchParams(window.location.search).get("images") === "token" ? "image-test-token" : "", onUnauthorized: () => undefined };

function ImageResult({ id, enabled }: { id: string; enabled: boolean }) {
  const image = useAPIImage({ queryKey: keys.page("image-test", id), path: `/api/image-test/${id}`, enabled });
  return <>
    <output data-testid="image-status">{image.status}:{image.fetchStatus}{image.error instanceof APIError ? `:${image.error.status}:${image.error.code}` : ""}</output>
    {image.data && <img src={image.data} alt="测试图片" />}
  </>;
}

function ImageHarness() {
  const [enabled, setEnabled] = useState(false);
  const [visible, setVisible] = useState(true);
  const [id, setId] = useState("success");
  return <main>
    <button type="button" onClick={() => setEnabled(true)}>读取图片</button>
    <button type="button" onClick={() => setVisible(value => !value)}>切换图片显示</button>
    <select aria-label="图片来源" value={id} onChange={event => setId(event.target.value)}>
      <option value="success">成功</option><option value="denied">无权限</option><option value="slow">等待响应</option>
    </select>
    {visible && <ImageResult id={id} enabled={enabled} />}
  </main>;
}

function ModalHarness() {
  const [kind, setKind] = useState("");
  const [lastKey, setLastKey] = useState("");
  const [undone, setUndone] = useState(false);
  const input = useRef<HTMLInputElement>(null);
  const overlayOpen = useOverlayOpen();
  const toast = useToast();
  return <main>
    <button type="button" onClick={() => setKind("窄版弹窗")}>打开窄版</button>
    <button type="button" onClick={() => setKind("宽版弹窗")}>打开宽版</button>
    <button type="button" onClick={() => setKind("无说明弹窗")}>打开无说明</button>
    <output data-testid="modal-overlay">{String(overlayOpen)}</output>
    <output data-testid="modal-key">{lastKey}</output>
    <output data-testid="modal-undone">{String(undone)}</output>
    {kind && <ModalFrame wide={kind === "宽版弹窗"} onClose={() => setKind("")}
      onOpenAutoFocus={event => { event.preventDefault(); input.current?.focus(); }}
      onKeyDown={event => setLastKey(event.key)}>
      <ModalHeading title={kind} description={kind === "无说明弹窗" ? undefined : <>带有<strong>格式</strong>的说明</>} />
      <button type="button" onClick={() => toast({ text: "弹窗内提示", undo: () => setUndone(true) })}>显示提示</button>
      <input ref={input} aria-label="弹窗输入" />
      <button type="button" onClick={() => setKind("")}>关闭弹窗</button>
    </ModalFrame>}
  </main>;
}

const byteCases = [0, 1.5, 1023, 1024, 1536, 10239, 10240, 1048575, 1048576, 1610612736, -1, NaN, Infinity, -Infinity];

function BytesHarness() {
  return <table aria-label="字节格式化"><tbody>{byteCases.map(bytes => <tr key={String(bytes)} data-testid="byte-case">
    <td>{String(bytes)}</td><td>{dictionaryBytes(bytes)}</td><td>{communityBytes(bytes)}</td><td>{releaseBytes(bytes)}</td>
  </tr>)}</tbody></table>;
}

function CountBarHarness() {
  return <section aria-label="计数占比条" className="w-[240px]">
    <div data-testid="count-zero"><CountBar value={0} max={0} /></div>
    <div data-testid="count-half"><CountBar value={1000} max={2000} locale="en-US" /></div>
    <div data-testid="count-full"><CountBar value={2000} max={2000} /></div>
    <div data-testid="count-over"><CountBar value={3000} max={2000} tone="warn" barClassName="rounded-full" valueClassName="text-[12.5px] text-body" /></div>
    <div data-testid="count-bad"><CountBar value={3} max={12} tone="bad" className="gap-2" /></div>
    <div data-testid="count-long"><CountBar value={1000000000} max={2000000000} /></div>
  </section>;
}

function AuthHarness() {
  const auth = useAuth();
  const api = useAPI();
  const toast = useToast();
  const [token, setToken] = useState("test-admin-token");
  const [result, setResult] = useState("");
  const [commits, setCommits] = useState(0);
  const mutation = useMutation({
    mutationFn: (path: string) => api.post(`auth-test/${path}`, z.unknown()),
  });
  const request = (path: string) => {
    setResult("");
    void mutation.mutateAsync(path).then(() => setResult(`${path}:成功`), error => setResult(`${path}:${error instanceof APIError ? String(error.status) : "请求失败"}`));
  };
  return <main>
    <output data-testid="auth-status">{auth.loading ? "加载中" : auth.authenticated ? "已登录" : "未登录"}</output>
    <output data-testid="auth-error">{auth.error}</output>
    <output data-testid="auth-result">{result}</output>
    <output data-testid="auth-commits">{commits}</output>
    <input aria-label="测试密钥" value={token} onChange={event => setToken(event.target.value)} />
    <button type="button" onClick={() => { setResult(""); void auth.login(token).then(() => setResult("登录完成")); }}>测试登录</button>
    <button type="button" onClick={() => request("delayed")}>启动旧请求</button>
    <button type="button" onClick={() => request("unauthorized")}>使当前登录失效</button>
    <button type="button" onClick={() => toast({ text: "可撤销操作", undo: () => api.post("auth-test/undo", z.unknown()) })}>排队可撤销操作</button>
    <button type="button" onClick={() => toast({ text: "待提交", delayCommit: () => api.post("auth-test/delayed", z.unknown()).finally(() => setCommits(value => value + 1)) })}>排队延迟提交</button>
    <button type="button" onClick={() => { setResult("正在退出"); void auth.logout().then(() => setResult("退出完成")); }}>测试退出</button>
  </main>;
}

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
      <AreaChart data={series} xKey="day" series={[{ key: "a", label: "A" }, { key: "b", label: "B" }]} showYAxis yDomain={[0, 2000]} ariaLabel="面积图" />
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
    <BytesHarness />
    <CountBarHarness />
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
const params = new URLSearchParams(window.location.search);
createRoot(root).render(<StrictMode><ToastProvider>{params.has("images")
  ? <QueryClientProvider client={imageClient}><APICredentialsContext.Provider value={imageCredentials}><ImageHarness /></APICredentialsContext.Provider></QueryClientProvider>
  : params.has("modal")
  ? <ModalHarness />
  : params.has("auth")
  ? <QueryClientProvider client={authClient}><AuthProvider><AuthHarness /></AuthProvider></QueryClientProvider>
  : <ConfirmProvider><Harness /></ConfirmProvider>}</ToastProvider></StrictMode>);

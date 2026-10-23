import { formatNumber } from "../../utils/number";
import { formatHours } from "../../utils/duration";
import { useCallback, useEffect, useMemo, useState } from "react";
import type { ReactNode } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { UseQueryResult } from "@tanstack/react-query";
import { RefreshCw } from "lucide-react";
import { errorMessage, isGithubDisabled, useAPI } from "../../api/client";
import { keys } from "../../api/keys";
import { issueActionResultSchema, issueDetailSchema, issueKey, issuePath, issueRef, issuesSchema, parseIssueKey } from "../../api/issues";
import type { IssueAction, IssueDetail, IssueEvent, IssuePlatform, IssueRef, IssueRow, IssueState, IssueStateFilter } from "../../api/issues";
import { relativeTime } from "../../utils/time";
import { PageIntro } from "../../shell/page-intro";
import { usePageSearch, useSetPageSearch } from "../../shell/page-search";
import { noPermissionHint, usePermissions } from "../../shell/permissions";
import { Button } from "../../ui/button";
import { Banner } from "../../ui/card";
import { useConfirm } from "../../ui/confirm";
import { CellText, DataTable } from "../../ui/data-table";
import type { BatchAction, Column } from "../../ui/data-table";
import { DetailDrawer, DrawerComposer } from "../../ui/drawer";
import type { DrawerAction, DrawerField } from "../../ui/drawer";
import { FilterChips } from "../../ui/filter-chips";
import { Pill } from "../../ui/pill";
import type { Tone } from "../../ui/pill";
import { Segmented } from "../../ui/segmented";
import { StatGrid, StatTile } from "../../ui/stat-tile";
import { ErrorState, NotConfigured, Skeleton, SkeletonRows } from "../../ui/states";
import { useToast } from "../../ui/toast";

const stateLabel: Record<IssueState, [string, Tone]> = {
  new: ["待分诊", "warn"],
  triaged: ["已分类", "info"],
  done: ["已关闭", "mute"],
  dup: ["重复", "mute"],
};

const kindLabel: Record<string, [string, Tone]> = {
  bug: ["问题", "bad"],
  idea: ["建议", "info"],
  docs: ["文档", "mute"],
};

const stateFilters: { value: IssueStateFilter; label: string }[] = [
  { value: "open", label: "待处理" },
  { value: "new", label: "待分诊" },
  { value: "triaged", label: "已分类" },
  { value: "closed", label: "已关闭" },
  { value: "all", label: "全部" },
];

const replyTemplates = [
  { name: "需要日志", text: "感谢反馈！麻烦在 设置 → 关于 → 导出诊断日志 后附在这里，我们看一下。" },
  { name: "已在修复", text: "已确认问题，修复会在下个版本发布，届时这里会更新。" },
  { name: "无法复现", text: "我们这边暂时没有复现，能否补充一下系统版本和复现步骤？" },
  { name: "已加入计划", text: "这个建议很好，已加入开发计划。" },
];

const closeReasons: Record<string, string> = { completed: "（已完成）", not_planned: "（不计划处理）", duplicate: "（重复）" };

// Labels that the table already shows as type or state, so the 标签 line leaves them out.
const typeAndStateLabels = ["triaged", "duplicate", "bug", "enhancement", "feature", "idea", "documentation", "docs"];

function extraLabels(row: IssueRow, platforms: readonly IssuePlatform[]): string[] {
  const hidden = new Set([...typeAndStateLabels, ...platforms.map(p => p.label.toLowerCase())]);
  return row.labels.filter(label => !hidden.has(label.toLowerCase()));
}

function eventText(event: IssueEvent): string {
  const who = event.actor ? `@${event.actor} ` : "";
  switch (event.kind) {
    case "created": return `${who}创建了 Issue`;
    case "commented": return `${who}回复：${event.text}`;
    case "labeled": return `${who}添加标签「${event.text}」`;
    case "unlabeled": return `${who}移除标签「${event.text}」`;
    case "assigned": return `${who}指派给 @${event.text}`;
    case "unassigned": return `${who}取消指派 @${event.text}`;
    case "closed": return `${who}关闭了 Issue${closeReasons[event.text] ?? ""}`;
    case "reopened": return `${who}重新打开了 Issue`;
    case "renamed": return `${who}将标题改为「${event.text}」`;
    case "marked_as_duplicate": return `${who}标记为重复`;
    default: return `${who}${event.kind}`;
  }
}

const isOpenRow = (row: IssueRow) => row.state === "new" || row.state === "triaged";

type ActionInput = { action: IssueAction; items: IssueRef[]; body?: string };
type ReplyDraft = { issueKey: string; body: string };

export default function IssuesPage() {
  const api = useAPI();
  const client = useQueryClient();
  const toast = useToast();
  const confirm = useConfirm();
  const { can } = usePermissions();
  const canTriage = can("triage_issues");
  const writeHint = canTriage ? undefined : noPermissionHint;
  const { focus } = usePageSearch();
  const setSearch = useSetPageSearch();
  const [platform, setPlatform] = useState("all");
  const [state, setState] = useState<IssueStateFilter>("open");
  const [page, setPage] = useState(1);
  const [openKey, setOpenKey] = useState<string | null>(null);
  const [reply, setReply] = useState<ReplyDraft | null>(null);

  const list = useQuery({
    queryKey: keys.page("issues", "list", { platform, state, page }),
    queryFn: ({ signal }) => api.get(`issues?platform=${encodeURIComponent(platform)}&state=${state}&page=${page}`, issuesSchema, { signal }),
    placeholderData: previous => previous,
  });
  const data = list.data;
  const platforms = useMemo(() => data?.platforms ?? [], [data?.platforms]);
  const platformName = useCallback((id: string) => platforms.find(p => p.id === id)?.name ?? "", [platforms]);
  const multiRepo = (data?.repos.length ?? 0) > 1;

  // Global search and notifications open an issue with ?focus=owner/repo#n.
  useEffect(() => {
    if (focus && parseIssueKey(focus)) setOpenKey(focus);
  }, [focus]);
  const openRef = openKey ? parseIssueKey(openKey) : null;
  const openIssue = useCallback((key: string) => {
    setReply(null);
    setOpenKey(key);
  }, []);
  const closeDrawer = () => {
    setOpenKey(null);
    setReply(null);
    if (focus) setSearch({ focus: undefined });
  };

  const detail = useQuery({
    queryKey: keys.page("issues", "detail", openKey),
    queryFn: ({ signal }) => api.get(issuePath(openRef as IssueRef), issueDetailSchema, { signal }),
    enabled: Boolean(openRef),
  });

  const refresh = useCallback(() => Promise.all([
    client.invalidateQueries({ queryKey: keys.page("issues") }),
    client.invalidateQueries({ queryKey: keys.shell }),
  ]), [client]);

  const mutation = useMutation({
    mutationFn: (input: ActionInput) => api.post("issues/actions", issueActionResultSchema, input),
    onSettled: () => refresh(),
  });
  const runAction = mutation.mutateAsync;

  // apply 执行操作并提示结果；只对确认成功且允许撤销的条目提供反向操作。没有适用条目时返回 false，保留批量选择。
  const apply = useCallback(async (action: IssueAction, rows: readonly IssueRow[], text: (count: number) => string) => {
    if (rows.length === 0) {
      toast("所选 Issue 都不适用这个操作");
      return false;
    }
    const result = await runAction({ action, items: rows.map(issueRef) });
    const failed = new Set(result.failed.map(item => `${item.repo}#${item.n}`));
    const uncertain = result.failed.some(item => item.code === "github_uncertain");
    const message = (failed.size ? `${text(result.affected)}，${failed.size} 个失败` : text(result.affected))
      + (uncertain ? "；部分操作结果未确认，请先在 GitHub 核对后再重试" : "");
    const targets = new Set(rows.map(row => `${row.repo}#${row.number}`));
    const expectedUndo = action === "triage" ? "untriage" : action === "close" || action === "mark_dup" ? "reopen" : null;
    const undo = result.undo?.action === expectedUndo && result.undo.items.every(item => targets.has(`${item.repo}#${item.n}`) && !failed.has(`${item.repo}#${item.n}`)) ? result.undo : null;
    if (undo) toast({
      text: message,
      undo: async () => {
        const undone = await runAction({ action: undo.action, items: undo.items });
        if (undone.failed.some(item => item.code === "github_uncertain")) throw new Error("部分操作结果未确认，请先在 GitHub 核对后再重试");
        if (undone.failed.some(item => item.code === "conflict")) throw new Error(`已撤销 ${undone.affected} 个，部分 Issue 已被修改，请核对最新状态`);
        if (undone.failed.length) throw new Error(`已撤销 ${undone.affected} 个，${undone.failed.length} 个失败，请核对后重试`);
      },
    });
    else toast(message);
    return true;
  }, [runAction, toast]);

  // guarded shows a failure as a toast and resolves to false, which keeps a batch selection for another try.
  const guarded = useCallback(async (work: () => Promise<boolean>) => {
    try {
      return await work();
    } catch (error) {
      toast(`操作失败：${errorMessage(error)}`);
      return false;
    }
  }, [toast]);

  const triage = useCallback((rows: readonly IssueRow[]) => guarded(() => apply("triage", rows,
    count => rows.length === 1 ? `#${rows[0].number} 已分类` : `已分类 ${count} 个 Issue`)), [apply, guarded]);
  const markDup = useCallback((rows: readonly IssueRow[]) => guarded(() => apply("mark_dup", rows,
    count => rows.length === 1 ? `#${rows[0].number} 已标记为重复` : `已将 ${count} 个 Issue 标记为重复`)), [apply, guarded]);
  const close = useCallback(async (rows: readonly IssueRow[]) => {
    if (rows.length === 0) return apply("close", rows, () => "");
    const ok = await confirm({ title: rows.length === 1 ? `关闭 #${rows[0].number}？` : `关闭 ${rows.length} 个 Issue？`, description: "提交者会收到关闭通知，之后可以重新打开。", okLabel: "关闭" });
    if (ok === null) return false;
    return guarded(() => apply("close", rows, count => rows.length === 1 ? `#${rows[0].number} 已关闭` : `已关闭 ${count} 个 Issue`));
  }, [apply, confirm, guarded]);
  const reopen = useCallback((rows: readonly IssueRow[]) => guarded(() => apply("reopen", rows,
    count => rows.length === 1 ? `#${rows[0].number} 已重新打开` : `已重新打开 ${count} 个 Issue`)), [apply, guarded]);

  const columns = useMemo<Column<IssueRow>[]>(() => [
    {
      id: "title", header: "Issue", width: "minmax(260px,2.4fr)",
      cell: row => {
        const labels = extraLabels(row, platforms);
        const sub = [multiRepo ? row.repo.split("/")[1] : "", row.author, relativeTime(row.created_at), labels.length ? `标签：${labels.join("、")}` : ""].filter(Boolean).join(" · ");
        return <CellText title={`#${row.number}  ${row.title}`} sub={sub} />;
      },
    },
    { id: "platform", header: "平台", width: "110px", cell: row => platformName(row.platform) || <span className="text-muted">—</span> },
    { id: "kind", header: "类型", width: "90px", cell: row => kindLabel[row.kind] ? <Pill tone={kindLabel[row.kind][1]}>{kindLabel[row.kind][0]}</Pill> : <span className="text-muted">—</span> },
    { id: "state", header: "状态", width: "90px", cell: row => <Pill tone={stateLabel[row.state][1]}>{stateLabel[row.state][0]}</Pill> },
    {
      id: "actions", header: "", width: "190px", align: "right",
      cell: row => row.state === "new"
        ? <>
          <Button size="sm" disabled={!canTriage} title={writeHint} onClick={event => { event.stopPropagation(); void markDup([row]); }}>标记重复</Button>
          <Button size="sm" variant="primary" disabled={!canTriage} title={writeHint} onClick={event => { event.stopPropagation(); void triage([row]); }}>确认并分类</Button>
        </>
        : <Button size="sm" onClick={event => { event.stopPropagation(); openIssue(issueKey(row)); }}>详情</Button>,
    },
  ], [platforms, multiRepo, platformName, canTriage, writeHint, markDup, triage, openIssue]);

  const batchActions = useMemo<BatchAction<IssueRow>[]>(() => {
    const actions: BatchAction<IssueRow>[] = [
      { label: "确认并分类", variant: "primary", disabled: !canTriage, onClick: rows => triage(rows.filter(row => row.state === "new")) },
      { label: "标记重复", disabled: !canTriage, onClick: rows => markDup(rows.filter(row => row.state !== "dup")) },
      { label: "关闭", disabled: !canTriage, onClick: rows => close(rows.filter(isOpenRow)) },
    ];
    if (state === "closed" || state === "all") actions.push({ label: "重新打开", disabled: !canTriage, onClick: rows => reopen(rows.filter(row => !isOpenRow(row))) });
    return actions;
  }, [canTriage, triage, markDup, close, reopen, state]);

  const getRowId = useCallback((row: IssueRow) => issueKey(row), []);
  const searchText = useCallback((row: IssueRow) => `#${row.number} ${row.title} ${row.author} ${row.labels.join(" ")} ${platformName(row.platform)}`, [platformName]);

  const sendReply = useMutation({
    mutationFn: (v: { ref: IssueRef; draft: ReplyDraft }) => runAction({ action: "comment", items: [v.ref], body: v.draft.body.trim() }),
    onSuccess: (_result, v) => {
      // 仅清理本次提交的草稿对象，保留继续输入、重开抽屉或切换 Issue 后的新草稿。
      setReply(current => current === v.draft ? null : current);
      toast("回复已发送");
    },
    onError: error => toast(`发送失败：${errorMessage(error)}`),
  });

  if (list.isError && isGithubDisabled(list.error)) {
    return <>
      <PageIntro page="issues" />
      <ErrorState error={list.error} />
    </>;
  }
  if (data && data.repos.length === 0) {
    return <>
      <PageIntro page="issues" />
      <NotConfigured title="未配置 Issue 仓库">在 config.json 的 admin.github.issue_repos 中填写要分诊的仓库（例如 metasequoiaime/msime），平台由 admin.github.platforms 的 label 映射。</NotConfigured>
    </>;
  }

  const counts = data?.platform_counts ?? {};
  const chipOptions = [
    { key: "all", label: "全部", count: counts.all },
    ...platforms.map(p => ({ key: p.id, label: p.name, count: counts[p.id] })),
    ...((counts.other ?? 0) > 0 || platform === "other" ? [{ key: "other", label: "其他", count: counts.other }] : []),
  ];
  const stats = data?.stats;

  return <>
    <PageIntro page="issues">
      <Button size="sm" variant="outline" onClick={() => void refresh()} disabled={list.isFetching}>
        <RefreshCw size={14} aria-hidden="true" className={list.isFetching ? "animate-spin" : undefined} />刷新
      </Button>
    </PageIntro>
    <div className="grid gap-4">
      {data && data.unavailable.length > 0 && <Banner tone="warn">以下仓库暂时无法从 GitHub 读取，列表和统计不含它们：<span className="font-mono text-[12.5px]">{data.unavailable.join("、")}</span></Banner>}
      <StatGrid>
        {stats ? <>
          <StatTile label="待分诊" value={formatNumber(stats.pending)} />
          <StatTile label="已分类待处理" value={formatNumber(stats.triaged)} />
          <StatTile label="本周新增" value={formatNumber(stats.new_this_week)} />
          <StatTile label="平均首次响应" value={formatHours(stats.first_response_hours, "minutes")} sub={stats.first_response_samples ? `近 30 天 · ${stats.first_response_samples} 个 Issue` : "近 30 天暂无维护者回复"} />
        </> : ["a", "b", "c", "d"].map(key => <Skeleton key={key} className="h-[76px] rounded-2xl" />)}
      </StatGrid>
      <DataTable
        ariaLabel="Issue 列表"
        data={data?.items}
        loading={list.isPending}
        error={list.isError ? list.error : undefined}
        onRetry={() => void list.refetch()}
        columns={columns}
        getRowId={getRowId}
        selectable
        batchActions={batchActions}
        toolbar={<div className="flex flex-wrap items-center gap-2.5">
          <FilterChips label="平台" value={platform} onChange={key => { setPlatform(key); setPage(1); }} options={chipOptions} />
          <Segmented label="状态" size="sm" value={state} onChange={value => { setState(value); setPage(1); }} options={stateFilters} />
        </div>}
        searchText={searchText}
        onRowClick={row => openIssue(issueKey(row))}
        emptyText={state === "new" || state === "open" ? "没有待处理的 Issue" : "这一栏是空的"}
        minWidth="800px"
        pagination={{ page, total: data?.total ?? 0, pageSize: data?.page_size ?? 50, onPageChange: setPage }}
      />
    </div>
    {openRef && openKey && <IssueDrawer
      onClose={closeDrawer}
      fallback={data?.items.find(row => issueKey(row) === openKey)}
      detail={detail}
      platformName={platformName}
      platforms={platforms}
      canTriage={canTriage}
      onOpenIssue={openIssue}
      onTriage={row => void triage([row])}
      onCloseIssue={row => void close([row])}
      onReopen={row => void reopen([row])}
      composer={<DrawerComposer value={reply && reply.issueKey === openKey ? reply.body : ""} onChange={body => setReply({ issueKey: openKey, body })} templates={replyTemplates} sending={sendReply.isPending}
        onSend={() => {
          if (!canTriage) {
            toast(noPermissionHint);
            return;
          }
          if (!reply || reply.issueKey !== openKey || !reply.body.trim()) return;
          sendReply.mutate({ ref: openRef, draft: reply });
        }} />}
    />}
  </>;
}

function IssueDrawer({ onClose, fallback, detail, platformName, platforms, canTriage, onOpenIssue, onTriage, onCloseIssue, onReopen, composer }: {
  onClose: () => void;
  // fallback is the listed row, shown while the detail loads.
  fallback?: IssueRow;
  detail: UseQueryResult<IssueDetail>;
  platformName: (id: string) => string;
  platforms: readonly IssuePlatform[];
  canTriage: boolean;
  onOpenIssue: (key: string) => void;
  onTriage: (row: IssueRow) => void;
  onCloseIssue: (row: IssueRow) => void;
  onReopen: (row: IssueRow) => void;
  composer: ReactNode;
}) {
  const data = detail.data;
  const row: IssueRow | undefined = data?.issue ?? fallback;
  const status = <>
    {detail.isError && <ErrorState className="mb-4" error={detail.error} onRetry={() => void detail.refetch()} />}
    {detail.isPending && <SkeletonRows rows={4} className="mb-2 p-0" />}
  </>;
  if (!row) return <DetailDrawer open onClose={onClose} title="Issue 详情">{status}</DetailDrawer>;

  const labels = extraLabels(row, platforms);
  let assignees = "未指派";
  if (row.assignees.length) assignees = row.assignees.map(a => `@${a}`).join("、");
  else if (data?.platform_assignee && row.state === "new") assignees = `未指派（分类后指派给 @${data.platform_assignee}）`;
  let similar: ReactNode = "…";
  if (data) {
    similar = data.similar.length
      ? <span className="flex flex-wrap gap-x-2 gap-y-1">{data.similar.map(item => <button key={issueKey(item)} type="button" title={item.title} onClick={() => onOpenIssue(issueKey(item))} className="text-accent-ink hover:underline">
        #{item.number}（{stateLabel[item.state][0]}）
      </button>)}</span>
      : "无";
  }
  const fields: DrawerField[] = [
    { label: "平台", value: platformName(row.platform) || "—" },
    { label: "仓库", value: row.repo, mono: true },
    { label: "标签", value: labels.length ? labels.join("、") : "无" },
    { label: "来源", value: "GitHub" },
    { label: "指派给", value: assignees },
    { label: "相似 Issue", value: similar },
  ];
  const actions: DrawerAction[] = [{ label: "在 GitHub 打开", onClick: () => window.open(row.url, "_blank", "noopener,noreferrer") }];
  if (row.state === "new") actions.push({ label: "确认并分类", variant: "primary", disabled: !canTriage, onClick: () => onTriage(row) });
  else if (row.state === "triaged") actions.push({ label: "关闭", variant: "danger", disabled: !canTriage, onClick: () => onCloseIssue(row) });
  else actions.push({ label: "重新打开", disabled: !canTriage, onClick: () => onReopen(row) });
  const kind = kindLabel[row.kind];
  return <DetailDrawer
    open
    onClose={onClose}
    title={`#${row.number} ${row.title}`}
    sub={`${row.author} · ${relativeTime(row.created_at)}`}
    pills={[...(kind ? [{ text: kind[0], tone: kind[1] }] : []), { text: stateLabel[row.state][0], tone: stateLabel[row.state][1] }]}
    fields={fields}
    sections={data ? [
      { title: "描述", items: [{ text: data.issue.body.trim() || "（提交者没有填写描述）" }] },
      { title: data.timeline_truncated ? "时间线（仅最近部分，完整记录见 GitHub）" : "时间线", items: data.timeline.map(event => ({ text: eventText(event), meta: relativeTime(event.at) })), empty: "暂无记录" },
    ] : []}
    actions={actions}
    composer={composer}
  >
    {status}
  </DetailDrawer>;
}

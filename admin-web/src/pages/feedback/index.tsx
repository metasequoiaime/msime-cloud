import { useCallback, useMemo } from "react";
import { keepPreviousData, useQuery, useQueryClient } from "@tanstack/react-query";
import { errorMessage, useAPI } from "../../api/client";
import { platformName } from "../../api/crash";
import { feedbackListSchema, feedbackPlatforms, feedbackStatusLabels, feedbackStatusTones, feedbackStatuses, feedbackTypeLabels, feedbackTypes } from "../../api/feedback";
import type { FeedbackRow } from "../../api/feedback";
import { keys } from "../../api/keys";
import { relativeTime } from "../../shell/notifications";
import { PageIntro } from "../../shell/page-intro";
import { usePageSearch, useSetPageSearch } from "../../shell/page-search";
import { noPermissionHint, usePermissions } from "../../shell/permissions";
import { Button } from "../../ui/button";
import { CellText, DataTable } from "../../ui/data-table";
import type { Column } from "../../ui/data-table";
import { FilterChips } from "../../ui/filter-chips";
import type { ChipOption } from "../../ui/filter-chips";
import { Pill } from "../../ui/pill";
import { useToast } from "../../ui/toast";
import { FeedbackDrawer } from "./drawer";

const ALL = "all";

// 列表里只显示正文第一行，全文和诊断信息在抽屉里看。
const firstLine = (text: string) => text.split("\n")[0].trim();

export default function FeedbackPage() {
  const api = useAPI();
  const client = useQueryClient();
  const toast = useToast();
  const { can } = usePermissions();
  const canTriage = can("triage_issues");
  const { type = ALL, status = ALL, platform = ALL, page: rawPage, focus } = usePageSearch();
  const setSearch = useSetPageSearch();
  const page = Math.max(1, Math.min(10000, Number.parseInt(rawPage ?? "1", 10) || 1));

  const list = useQuery({
    queryKey: keys.page("feedback", "list", { type, status, platform, page }),
    queryFn: ({ signal }) => {
      const params = new URLSearchParams({ page: String(page) });
      if (type !== ALL) params.set("type", type);
      if (status !== ALL) params.set("status", status);
      if (platform !== ALL) params.set("platform", platform);
      return api.get(`feedback?${params}`, feedbackListSchema, { signal });
    },
    placeholderData: keepPreviousData,
  });

  // 标记已处理和重新打开互为撤销。
  const setStatus = useCallback(async (row: FeedbackRow, resolved: boolean) => {
    const run = async (to: boolean) => {
      try {
        await api.action({ action: to ? "resolve_feedback" : "reopen_feedback", id: row.id });
      } finally {
        await client.invalidateQueries({ queryKey: keys.page("feedback") });
      }
    };
    try {
      await run(resolved);
      toast({ text: resolved ? "已标记为已处理" : "已重新打开", undo: () => run(!resolved) });
    } catch (error) {
      toast(`操作失败：${errorMessage(error)}`);
    }
  }, [api, client, toast]);

  const open = useCallback((row: FeedbackRow) => setSearch({ focus: row.id }, { replace: false }), [setSearch]);
  const columns = useMemo<Column<FeedbackRow>[]>(() => [
    { id: "text", header: "反馈内容", width: "minmax(260px,2.4fr)", cell: row => <CellText title={<span title={row.text}>{firstLine(row.text)}</span>} sub={row.screenshots > 0 ? `${row.author} · ${row.screenshots} 张截图` : row.author} /> },
    { id: "type", header: "类型", width: "80px", cell: row => <Pill tone="mute">{feedbackTypeLabels[row.type]}</Pill> },
    { id: "platform", header: "平台 / 版本", width: "120px", cell: row => <CellText strong={false} title={platformName(row.platform)} sub={<span className="font-mono">{row.app_version}</span>} /> },
    { id: "time", header: "提交时间", width: "100px", cell: row => <span className="text-muted">{relativeTime(row.created_at)}</span> },
    { id: "status", header: "状态", width: "90px", cell: row => <Pill tone={feedbackStatusTones[row.status]}>{feedbackStatusLabels[row.status]}</Pill> },
    {
      id: "actions", header: "", width: "120px", align: "right",
      cell: row => <Button size="sm" variant="outline" disabled={!canTriage} title={canTriage ? undefined : noPermissionHint}
        onClick={event => { event.stopPropagation(); void setStatus(row, row.status === "new"); }}>{row.status === "new" ? "标记已处理" : "重新打开"}</Button>,
    },
  ], [canTriage, setStatus]);

  const typeChips: ChipOption<string>[] = [{ key: ALL, label: "全部类型" }, ...feedbackTypes.map(key => ({ key, label: feedbackTypeLabels[key] }))];
  const statusChips: ChipOption<string>[] = [{ key: ALL, label: "全部状态" }, ...feedbackStatuses.map(key => ({ key, label: feedbackStatusLabels[key] }))];
  const platformChips: ChipOption<string>[] = [{ key: ALL, label: "全部平台" }, ...feedbackPlatforms.map(key => ({ key, label: platformName(key) }))];
  const rows = list.data?.items;
  const focused = rows?.find(row => row.id === focus);

  return <>
    <PageIntro page="feedback" />
    <DataTable
      ariaLabel="用户反馈"
      data={rows}
      loading={list.isPending}
      error={list.error}
      onRetry={() => void list.refetch()}
      columns={columns}
      getRowId={row => row.id}
      onRowClick={open}
      toolbar={<div className="flex flex-wrap items-center gap-3">
        <FilterChips label="状态" value={status} options={statusChips} onChange={next => setSearch({ status: next === ALL ? undefined : next, page: undefined })} />
        <FilterChips label="类型" value={type} options={typeChips} onChange={next => setSearch({ type: next === ALL ? undefined : next, page: undefined })} />
        <FilterChips label="平台" value={platform} options={platformChips} onChange={next => setSearch({ platform: next === ALL ? undefined : next, page: undefined })} />
      </div>}
      searchText={row => `${row.text} ${row.author} ${row.app_version} ${row.edition} ${row.id}`}
      emptyText={type === ALL && status === ALL && platform === ALL ? "还没有用户反馈" : "没有符合筛选条件的反馈"}
      minWidth="820px"
      pagination={{ page, total: list.data?.total ?? 0, pageSize: 50, onPageChange: next => setSearch({ page: next > 1 ? String(next) : undefined }) }}
    />
    <FeedbackDrawer row={focused} open={Boolean(focus)} loading={list.isPending} canTriage={canTriage} onClose={() => setSearch({ focus: undefined })} onStatus={setStatus} />
  </>;
}

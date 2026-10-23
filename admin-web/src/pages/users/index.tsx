import { parsePage } from "../../utils/pagination";
import { formatNumber } from "../../utils/number";
import { useCallback, useMemo } from "react";
import { keepPreviousData, useQuery } from "@tanstack/react-query";
import { useAPI } from "../../api/client";
import { keys } from "../../api/keys";
import { userStatsSchema, usersSchema } from "../../api/users";
import type { UserRow } from "../../api/users";
import { PageIntro } from "../../shell/page-intro";
import { usePageSearch, useSetPageSearch } from "../../shell/page-search";
import { noPermissionHint, usePermissions } from "../../shell/permissions";
import { Button } from "../../ui/button";
import { CellText, DataTable } from "../../ui/data-table";
import type { Column } from "../../ui/data-table";
import { FilterChips } from "../../ui/filter-chips";
import type { ChipOption } from "../../ui/filter-chips";
import { Pill } from "../../ui/pill";
import { StatGrid, StatTile } from "../../ui/stat-tile";
import { useUserActions } from "./actions";
import { activeText, contactText, displayName, roleOf, userRoles } from "./labels";
import { UserDrawer } from "./user-drawer";

// The design's chips are 全部 / 维护者 / 审核志愿者 / 普通用户; operator and read-only members get a chip only when such accounts exist.
const baseRoles = ["maintainer", "reviewer", "user"] as const;
const optionalRoles = ["operator", "readonly"] as const;

export default function UsersPage() {
  const api = useAPI();
  const { can } = usePermissions();
  const canBan = can("ban_users");
  const actions = useUserActions();
  const { role = "all", page: rawPage, focus } = usePageSearch();
  const setSearch = useSetPageSearch();
  const page = parsePage(rawPage);

  const stats = useQuery({
    queryKey: keys.page("users", "stats"),
    queryFn: ({ signal }) => api.get("users/stats", userStatsSchema, { signal }),
  });
  const list = useQuery({
    queryKey: keys.page("users", "list", { role, page }),
    queryFn: ({ signal }) => {
      const params = new URLSearchParams({ page: String(page) });
      if (role !== "all") params.set("role", role);
      return api.get(`users?${params}`, usersSchema, { signal });
    },
    placeholderData: keepPreviousData,
  });

  const open = useCallback((row: UserRow) => setSearch({ focus: row.id }, { replace: false }), [setSearch]);
  const columns = useMemo<Column<UserRow>[]>(() => [
    { id: "account", header: "账号", width: "minmax(220px,2fr)", cell: row => <CellText title={displayName(row)} sub={contactText(row)} /> },
    { id: "role", header: "角色", width: "110px", cell: row => <Pill tone={roleOf(row.role).tone}>{roleOf(row.role).label}</Pill> },
    { id: "devices", header: "设备", width: "100px", cell: row => <span className="tabular-nums">{row.devices} 台设备</span> },
    { id: "active", header: "最近活跃", width: "100px", cell: row => <span className="text-muted">{activeText(row.last_active)}</span> },
    {
      id: "actions", header: "", width: "180px", align: "right", cell: row => {
        const target = { id: row.id, name: displayName(row) };
        return <>
          <Button size="sm" variant="outline" onClick={event => { event.stopPropagation(); open(row); }}>查看</Button>
          <Button size="sm" variant="outline" disabled={!canBan} title={canBan ? undefined : noPermissionHint}
            onClick={event => { event.stopPropagation(); void (row.banned ? actions.unban(target) : actions.ban(target)); }}>
            {row.banned ? "解除封禁" : "封禁"}
          </Button>
        </>;
      },
    },
  ], [actions, canBan, open]);

  const s = stats.data;
  const roleCounts = s?.roles ?? {};
  const chips: ChipOption<string>[] = [
    { key: "all", label: "全部", count: s?.total },
    ...baseRoles.map(key => ({ key, label: userRoles[key].label, count: s ? roleCounts[key] ?? 0 : undefined })),
    ...optionalRoles.filter(key => (roleCounts[key] ?? 0) > 0 || role === key).map(key => ({ key, label: userRoles[key].label, count: roleCounts[key] ?? 0 })),
  ];

  return <>
    <PageIntro page="users" />
    <StatGrid className="mb-4">
      <StatTile label="注册账号" value={s ? formatNumber(s.total) : "—"} />
      <StatTile label="本周新增" value={s ? `+${formatNumber(s.new_7d)}` : "—"} sub="近 7 天注册" />
      <StatTile label="开启设置同步" value={s ? `${Math.round(s.sync_ratio * 100)}%` : "—"} sub="上传过设置的账号占比" />
      <StatTile label="已封禁" value={s ? formatNumber(s.banned) : "—"} />
    </StatGrid>
    <DataTable
      ariaLabel="用户列表"
      data={list.data?.items}
      loading={list.isPending}
      error={list.error}
      onRetry={() => void list.refetch()}
      columns={columns}
      getRowId={row => row.id}
      onRowClick={open}
      toolbar={<FilterChips label="角色" value={role} options={chips} onChange={next => setSearch({ role: next === "all" ? undefined : next, page: undefined })} />}
      searchText={row => `${displayName(row)} ${row.contact} ${row.id}`}
      emptyText={role === "all" ? "还没有注册用户" : "没有这个角色的账号"}
      minWidth="720px"
      pagination={{ page, total: list.data?.total ?? 0, pageSize: 50, onPageChange: next => setSearch({ page: next > 1 ? String(next) : undefined }) }}
    />
    <UserDrawer id={focus} onClose={() => setSearch({ focus: undefined })} actions={actions} />
  </>;
}

import { formatNumber } from "../utils/number";
import { useEffect, useMemo, useState } from "react";
import type { ReactNode } from "react";
import { createColumnHelper, rowSelectionFeature, tableFeatures, useTable } from "@tanstack/react-table";
import type { RowData, RowSelectionState } from "@tanstack/react-table";
import { errorMessage } from "../api/client";
import { Button } from "./button";
import type { ButtonProps } from "./button";
import { cn } from "./cn";
import { Empty, ErrorState, SkeletonRows } from "./states";
import { useToast } from "./toast";

export type Column<T> = {
  id: string;
  header: ReactNode;
  cell: (row: T) => ReactNode;
  // width is a CSS grid track, e.g. "110px" or "minmax(260px,2.4fr)"; defaults to minmax(0,1fr).
  width?: string;
  align?: "left" | "right";
  className?: string;
};

export type BatchAction<T> = {
  label: string;
  variant?: ButtonProps["variant"];
  disabled?: boolean;
  // onClick receives the selected rows on the current page. The selection is cleared afterwards unless it returns (or resolves to) false, e.g. when a confirm dialog was cancelled. A rejection is shown as an 操作失败 toast and keeps the selection.
  onClick: (rows: T[]) => unknown;
};

export type ServerPagination = {
  // page is 1-based.
  page: number;
  pageSize?: number;
  total: number;
  onPageChange: (page: number) => void;
};

export type DataTableProps<T> = {
  data: readonly T[] | undefined;
  // columns must be referentially stable (module scope or useMemo).
  columns: readonly Column<T>[];
  getRowId: (row: T) => string;
  selectable?: boolean;
  batchActions?: readonly BatchAction<T>[];
  onRowClick?: (row: T) => void;
  // toolbar is rendered on the left of the toolbar row, usually <FilterChips>.
  toolbar?: ReactNode;
  // searchText enables the 在本页筛选… input; it returns the text a row is matched against (case-insensitive).
  searchText?: (row: T) => string;
  emptyText?: string;
  loading?: boolean;
  error?: unknown;
  onRetry?: () => void;
  pagination?: ServerPagination;
  // minWidth keeps the grid readable on narrow screens; the card scrolls horizontally below it.
  minWidth?: string;
  ariaLabel: string;
  className?: string;
};

const features = tableFeatures({ rowSelectionFeature });
const EMPTY: never[] = [];

export function DataTable<T extends RowData>({ data, columns, getRowId, selectable = false, batchActions = [], onRowClick, toolbar, searchText, emptyText = "这一栏是空的", loading = false, error, onRetry, pagination, minWidth = "720px", ariaLabel, className }: DataTableProps<T>) {
  const [query, setQuery] = useState("");
  const [rowSelection, setRowSelection] = useState<RowSelectionState>({});
  const [busy, setBusy] = useState(false);
  const toast = useToast();
  const q = query.trim().toLowerCase();
  const rows = useMemo(() => {
    const all = data ?? (EMPTY as T[]);
    return q && searchText ? all.filter(row => searchText(row).toLowerCase().includes(q)) : all as T[];
  }, [data, q, searchText]);

  const tableColumns = useMemo(() => {
    const helper = createColumnHelper<typeof features, T>();
    return columns.map(column => helper.display({ id: column.id, header: () => column.header, cell: ({ row }) => column.cell(row.original) }));
  }, [columns]);

  const table = useTable({
    features,
    columns: tableColumns,
    data: rows,
    getRowId: row => getRowId(row),
    enableRowSelection: selectable,
    state: { rowSelection },
    onRowSelectionChange: updater => setRowSelection(previous => typeof updater === "function" ? updater(previous) : updater),
  }, state => ({ rowSelection: state.rowSelection }));

  // Selection never outlives the rows it refers to: drop ids that left the current page, filter or data set.
  useEffect(() => {
    setRowSelection(previous => {
      const visible = new Set(rows.map(row => getRowId(row)));
      const next: RowSelectionState = {};
      let changed = false;
      for (const [id, selected] of Object.entries(previous)) {
        if (selected && visible.has(id)) next[id] = true;
        else changed = true;
      }
      return changed ? next : previous;
    });
  }, [rows, getRowId]);

  const page = pagination?.page;
  useEffect(() => { void page; setRowSelection({}); }, [page]);

  const tableRows = table.getRowModel().rows;
  const selectedRows = tableRows.filter(row => row.getIsSelected()).map(row => row.original);
  const selectedCount = selectedRows.length;
  const template = [selectable ? "20px" : "", ...columns.map(column => column.width ?? "minmax(0,1fr)")].filter(Boolean).join(" ");
  const total = pagination ? pagination.total : rows.length;
  const pageSize = pagination?.pageSize ?? 50;
  const pages = pagination ? Math.max(1, Math.ceil(pagination.total / pageSize)) : 1;

  const runBatch = async (action: BatchAction<T>) => {
    setBusy(true);
    try {
      const result = await action.onClick(selectedRows);
      if (result !== false) setRowSelection({});
    } catch (error) {
      toast(`操作失败：${errorMessage(error)}`);
    } finally {
      setBusy(false);
    }
  };

  const span = columns.length + (selectable ? 1 : 0);
  const fullRow = (content: ReactNode) => <tr className="block"><td className="block" colSpan={span}>{content}</td></tr>;
  let body: ReactNode;
  if (error) body = fullRow(<div className="p-4"><ErrorState error={error} onRetry={onRetry} /></div>);
  else if (loading && !data) body = fullRow(<SkeletonRows />);
  else if (tableRows.length === 0) body = fullRow(<Empty title={q ? `没有匹配「${query.trim()}」的结果` : emptyText} />);
  else body = tableRows.map(row => {
    const clickable = Boolean(onRowClick);
    return <tr key={row.id} aria-selected={selectable ? row.getIsSelected() : undefined} tabIndex={clickable ? 0 : undefined}
      onClick={clickable ? () => onRowClick?.(row.original) : undefined}
      onKeyDown={clickable ? event => { if (event.key === "Enter" && event.target === event.currentTarget) onRowClick?.(row.original); } : undefined}
      className={cn("grid items-center gap-4 border-t border-hair px-4 py-3 transition-colors first:border-t-0 hover:bg-panel-2", clickable && "cursor-pointer", row.getIsSelected() && "bg-accent-soft hover:bg-accent-soft")}
      style={{ gridTemplateColumns: template }}>
      {selectable && <td className="flex items-center">
        <input type="checkbox" aria-label="选择此行" checked={row.getIsSelected()} disabled={!row.getCanSelect()} onClick={event => event.stopPropagation()} onChange={row.getToggleSelectedHandler()} />
      </td>}
      {row.getAllCells().map((cell, index) => {
        const column = columns[index];
        return <td key={cell.id} className={cn("min-w-0 text-[13.5px] text-body", column?.align === "right" && "flex justify-end gap-1.5 text-right", column?.className)}><table.FlexRender cell={cell} /></td>;
      })}
    </tr>;
  });

  return <div className={cn("min-w-0 overflow-clip rounded-[18px] bg-panel ring-1 ring-hair", className)}>
    {(toolbar || searchText) && <div className="flex flex-wrap items-center gap-3 border-b border-hair px-3.5 py-3">
      {/* The toolbar slot keeps a real basis so filter chips take the first line on phones and the page filter wraps below them, instead of the chips collapsing into a narrow column. */}
      <div className="min-w-0 grow basis-[260px] max-[759px]:basis-full">{toolbar}</div>
      {searchText && <input type="search" value={query} onChange={event => setQuery(event.target.value)} placeholder="在本页筛选…" aria-label="在本页筛选"
        className="h-[30px] w-[180px] rounded-lg bg-panel-2 px-2.5 text-[13px] text-ink outline-none placeholder:text-muted focus:ring-[1.5px] focus:ring-accent" />}
      <span className="text-[12.5px] whitespace-nowrap text-muted tabular-nums">共 {formatNumber(total)} 条</span>
    </div>}
    {selectable && selectedCount > 0 && <div className="sticky top-16 z-10 flex flex-wrap items-center gap-2 bg-accent-soft px-4 py-2.5 backdrop-blur-sm" role="toolbar" aria-label="批量操作">
      <span className="mr-1 text-[13px] font-semibold text-accent-ink">已选 {selectedCount} 项</span>
      {batchActions.map(action => <Button key={action.label} size="sm" variant={action.variant ?? "default"} disabled={busy || action.disabled} onClick={() => void runBatch(action)}>{action.label}</Button>)}
      <button type="button" className="ml-auto text-[13px] text-accent-ink hover:underline" onClick={() => setRowSelection({})}>取消选择</button>
    </div>}
    <div className="overflow-x-auto">
      <table aria-label={ariaLabel} className="block border-collapse text-left" style={{ minWidth }}>
        <thead className="block">
          <tr className="grid items-center gap-4 border-b border-hair px-4 py-2.5 text-xs font-semibold text-muted" style={{ gridTemplateColumns: template }}>
            {selectable && <th scope="col" className="flex items-center font-semibold">
              <input type="checkbox" title="全选" aria-label="全选" checked={tableRows.length > 0 && table.getIsAllRowsSelected()}
                ref={element => { if (element) element.indeterminate = table.getIsSomeRowsSelected(); }}
                onChange={table.getToggleAllRowsSelectedHandler()} disabled={tableRows.length === 0} />
            </th>}
            {columns.map(column => <th key={column.id} scope="col" className={cn("min-w-0 truncate font-semibold", column.align === "right" && "text-right")}>{column.header}</th>)}
          </tr>
        </thead>
        <tbody className="block">{body}</tbody>
      </table>
    </div>
    {pagination && pages > 1 && <div className="flex flex-wrap items-center justify-between gap-3 border-t border-hair px-4 py-3 text-[12.5px] text-muted">
      <span className="tabular-nums">第 {pagination.page} / {pages} 页 · 每页 {pageSize} 条</span>
      <div className="flex gap-2">
        <Button size="sm" variant="outline" disabled={pagination.page <= 1 || loading} onClick={() => pagination.onPageChange(pagination.page - 1)}>上一页</Button>
        <Button size="sm" variant="outline" disabled={pagination.page >= pages || loading} onClick={() => pagination.onPageChange(pagination.page + 1)}>下一页</Button>
      </div>
    </div>}
  </div>;
}

// CellText is the standard two-line cell: a bold title and a muted sub line.
export function CellText({ title, sub, mono = false, strong = true }: { title: ReactNode; sub?: ReactNode; mono?: boolean; strong?: boolean }) {
  return <div className="min-w-0">
    <div className={cn("truncate text-ink", strong && "font-semibold", mono && "font-mono text-[12.5px]")}>{title}</div>
    {sub && <div className="mt-0.5 truncate text-xs text-muted">{sub}</div>}
  </div>;
}

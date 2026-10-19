import { useCallback, useEffect, useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { useAPI } from "../../api/client";
import type { PRFilter, PRState, PRSummary } from "../../api/dictpr";
import { authorLabel, prDetailSchema, prListSchema, stateLabels, stateTones } from "../../api/dictpr";
import { keys } from "../../api/keys";
import { PageIntro } from "../../shell/page-intro";
import { usePageSearch, useSetPageSearch } from "../../shell/page-search";
import { relativeTime } from "../../utils/time";
import { buttonVariants } from "../../ui/button";
import { Card } from "../../ui/card";
import { cn } from "../../ui/cn";
import { FilterChips } from "../../ui/filter-chips";
import { Pill } from "../../ui/pill";
import { Empty, ErrorState, SkeletonRows } from "../../ui/states";
import { PRDetailCard } from "./detail";
import type { Overrides } from "./detail";

const filters: readonly PRFilter[] = ["open", "merged", "closed", "all"];
const filterLabels: Record<PRFilter, string> = { ...stateLabels, all: "全部" };

function isFilter(value: string | undefined): value is PRFilter {
  return filters.includes(value as PRFilter);
}

export default function DictprPage() {
  const api = useAPI();
  const search = usePageSearch();
  const setSearch = useSetPageSearch();
  const filter: PRFilter = isFilter(search.state) ? search.state : "open";
  // Delayed approvals and rejections show their outcome at once; the entry is dropped when the list is refetched or the action is undone.
  const [optimistic, setOptimistic] = useState<Record<number, PRState>>({});
  // Per pull request head, the reviewer's checkbox changes over the default (checked iff the entry is new).
  const [overrides, setOverrides] = useState<Overrides>({});

  const list = useQuery({
    queryKey: keys.page("dictpr", "list"),
    queryFn: ({ signal }) => api.get("dict-prs", prListSchema, { signal }),
  });

  const focus = Number(search.focus);
  const validFocus = Number.isInteger(focus) && focus > 0 && focus <= 2 ** 30 && String(focus) === search.focus;
  const recent = list.data?.items;
  const listedFocus = recent?.some(pr => pr.number === focus);
  const focused = useQuery({
    queryKey: keys.page("dictpr", "detail", focus),
    queryFn: ({ signal }) => api.get(`dict-prs/${focus}`, prDetailSchema, { signal }),
    enabled: validFocus && list.isSuccess && !list.isFetching && !listedFocus,
  });
  const historical = !listedFocus && validFocus ? focused.data?.pull : undefined;
  const items = useMemo(() => {
    const all = historical ? [...(recent ?? []), historical] : recent ?? [];
    return all.map(pr => optimistic[pr.number] ? { ...pr, state: optimistic[pr.number] } : pr);
  }, [recent, historical, optimistic]);
  const counts = useMemo(() => {
    const result: Record<PRFilter, number> = { open: 0, merged: 0, closed: 0, all: items.length };
    for (const pr of items) result[pr.state]++;
    return result;
  }, [items]);
  const visible = useMemo(() => filter === "all" ? items : items.filter(pr => pr.state === filter), [items, filter]);
  // 指定目标时只能展示该 PR；没有定位目标时，只从当前筛选结果中选择审核对象。
  const selected: PRSummary | undefined = search.focus ? (validFocus ? items.find(pr => pr.number === focus) : undefined) : visible[0];

  // A link from the global search or a notification may point at a pull request outside the current filter; show it under 全部 instead of silently opening another one.
  useEffect(() => {
    if (!list.data || !focus || visible.some(pr => pr.number === focus)) return;
    if (items.some(pr => pr.number === focus)) setSearch({ state: "all" });
  }, [list.data, focus, visible, items, setSearch]);

  const select = useCallback((pr: PRSummary) => setSearch({ focus: String(pr.number) }), [setSearch]);
  const step = useCallback((delta: number) => {
    if (!selected || visible.length === 0) return;
    const at = visible.findIndex(pr => pr.number === selected.number);
    const next = visible[Math.min(Math.max(at + delta, 0), visible.length - 1)];
    if (next) select(next);
  }, [selected, visible, select]);
  const markOptimistic = useCallback((number: number, state: PRState | null) => setOptimistic(previous => {
    const next = { ...previous };
    if (state) next[number] = state;
    else delete next[number];
    return next;
  }), []);

  const repo = list.data?.repo;
  const intro = <PageIntro page="dictpr">
    {repo && <a href={`https://github.com/${repo}/pulls`} target="_blank" rel="noopener noreferrer" className={buttonVariants({ variant: "outline", size: "sm" })}>在 GitHub 查看</a>}
  </PageIntro>;

  if (list.isError && !list.data) {
    return <>{intro}<ErrorState error={list.error} onRetry={() => list.refetch()} /></>;
  }

  return <>
    {intro}
    <div className="grid items-start gap-3.5 min-[1100px]:grid-cols-[340px_minmax(0,1fr)]">
      <Card className="overflow-hidden p-0">
        <div className="border-b border-hair p-3">
          <FilterChips label="PR 状态" value={filter} onChange={key => setSearch({ state: key, focus: undefined })}
            options={filters.map(key => ({ key, label: filterLabels[key], count: list.data ? counts[key] : undefined }))} />
        </div>
        {list.isPending ? <SkeletonRows rows={4} /> : visible.length === 0 ? <Empty title="这一栏是空的">{filter === "open" ? "官网有新的词库投稿时，会在这里出现待审核的 PR。" : undefined}</Empty> :
          <ul className="m-0 list-none p-0" aria-label="词库 PR 列表">
            {visible.map(pr => {
              const active = pr.number === selected?.number;
              return <li key={pr.number} className="border-b border-hair last:border-b-0">
                <button type="button" onClick={() => select(pr)} aria-current={active ? "true" : undefined}
                  className={cn("block w-full px-4 py-3.5 text-left transition hover:bg-panel-2", active && "bg-panel-2 shadow-[inset_3px_0_0_var(--accent)]")}>
                  <span className="flex min-w-0 items-baseline gap-2">
                    <span className="shrink-0 font-mono text-xs text-muted">#{pr.number}</span>
                    <span className="truncate font-semibold text-ink">{pr.title}</span>
                  </span>
                  <span className="mt-1.5 flex items-center justify-between gap-2 text-xs text-muted">
                    <span className="truncate">{authorLabel(pr)} · {relativeTime(pr.created_at)}</span>
                    <Pill tone={stateTones[pr.state]}>{stateLabels[pr.state]}</Pill>
                  </span>
                </button>
              </li>;
            })}
          </ul>}
        {list.isError && <ErrorState className="m-3" error={list.error} onRetry={() => list.refetch()} />}
      </Card>
      {selected ? <PRDetailCard key={selected.number} pr={selected} repo={repo ?? ""} overrides={overrides} setOverrides={setOverrides}
        onNext={() => step(1)} onPrevious={() => step(-1)} markOptimistic={markOptimistic} /> :
        <Card>{search.focus
          ? !validFocus ? <Empty title="定位的词库 PR 编号无效" />
            : list.isError ? <ErrorState error={list.error} onRetry={() => list.refetch()} />
              : focused.isError ? <ErrorState error={focused.error} onRetry={() => focused.refetch()} /> : <SkeletonRows rows={6} />
          : list.isPending ? <SkeletonRows rows={6} /> : <Empty title={items.length ? "当前状态下没有词库 PR" : "还没有词库 PR"}>{items.length ? "可切换状态查看其他投稿。" : "官网的词库投稿会以 GitHub PR 的形式出现在这里。"}</Empty>}</Card>}
    </div>
  </>;
}

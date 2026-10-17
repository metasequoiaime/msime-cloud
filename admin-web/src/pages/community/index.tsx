import { formatNumber } from "../../utils/number";
import { parsePage } from "../../utils/pagination";
import { useEffect, useRef, useState } from "react";
import type { ReactNode } from "react";
import { keepPreviousData, useQuery } from "@tanstack/react-query";
import { ToggleGroup } from "radix-ui";
import { Search } from "lucide-react";
import { useAPI } from "../../api/client";
import { keys } from "../../api/keys";
import type { Item, Section } from "../../api/community";
import { countsSchema, isSection, listSchema, sectionLabels, sections } from "../../api/community";
import { usePageSearch, useSetPageSearch } from "../../shell/page-search";
import { PageIntro } from "../../shell/page-intro";
import { usePermissions } from "../../shell/permissions";
import { Button } from "../../ui/button";
import { FilterChips } from "../../ui/filter-chips";
import { ErrorState, Skeleton } from "../../ui/states";
import { useModeration } from "./actions";
import type { Target } from "./actions";
import { ItemCard } from "./card";
import { ContentDrawer } from "./drawer";

type Status = "pending" | "approved" | "removed" | "all";
const statusOptions = [{ key: "pending", label: "待审核" }, { key: "approved", label: "已上架" }, { key: "removed", label: "已下架" }, { key: "all", label: "全部" }] as const;
const visibilityOptions = [{ key: "public", label: "公开图库" }, { key: "private", label: "私有库" }] as const;
const PAGE_SIZE = 50;

function isStatus(value: string | undefined): value is Status {
  return value === "pending" || value === "approved" || value === "removed" || value === "all";
}

// SectionTabs is the segmented tab row of the five content kinds, each with its pending count.
function SectionTabs({ value, onChange, pending }: { value: Section; onChange: (section: Section) => void; pending?: Record<Section, number> }) {
  return <div className="max-w-full overflow-x-auto">
    <ToggleGroup.Root type="single" value={value} aria-label="内容类型" onValueChange={next => { if (isSection(next)) onChange(next); }}
      className="inline-flex rounded-xl bg-panel-2 p-1">
      {sections.map(section => {
        const count = pending?.[section] ?? 0;
        return <ToggleGroup.Item key={section} value={section}
          className="inline-flex h-8 items-center gap-1.5 rounded-[9px] px-3.5 text-[13.5px] whitespace-nowrap text-muted transition hover:text-ink data-[state=on]:bg-panel data-[state=on]:font-semibold data-[state=on]:text-ink data-[state=on]:shadow-[0_1px_3px_rgba(0,0,0,.08)]">
          {sectionLabels[section]}
          {count > 0 && <span className="text-xs tabular-nums opacity-70">{count}</span>}
        </ToggleGroup.Item>;
      })}
    </ToggleGroup.Root>
  </div>;
}

export default function CommunityPage() {
  const api = useAPI();
  const { can } = usePermissions();
  const search = usePageSearch();
  const setSearch = useSetPageSearch();
  const tab: Section = isSection(search.tab) ? search.tab : "skins";
  const status: Status = isStatus(search.status) ? search.status : "pending";
  const visibility = search.visibility === "private" ? "private" : "public";
  const page = parsePage(search.page, Number.POSITIVE_INFINITY);
  const [typed, setTyped] = useState("");
  const [q, setQ] = useState("");
  const [open, setOpen] = useState<{ section: Section; id: string } | null>(null);
  const { approve, remove, restore, setCategory } = useModeration();
  const canReview = can("review_community");

  // The server search runs on what was typed after a short pause, not on every keystroke, and starts again from the first page.
  const applied = useRef("");
  useEffect(() => {
    const timer = window.setTimeout(() => {
      const next = typed.trim();
      if (next === applied.current) return;
      applied.current = next;
      setQ(next);
      setSearch({ page: undefined });
    }, 300);
    return () => window.clearTimeout(timer);
  }, [typed, setSearch]);

  // Global search and notifications link here with ?focus=<section>/<id> (or a bare id in the current tab).
  const focus = search.focus;
  useEffect(() => {
    if (!focus) return;
    const slash = focus.indexOf("/");
    const prefix = slash > 0 ? focus.slice(0, slash) : "";
    setOpen(isSection(prefix) ? { section: prefix, id: focus.slice(slash + 1) } : { section: tab, id: focus });
  }, [focus, tab]);

  const counts = useQuery({
    queryKey: keys.page("community", "counts"),
    queryFn: ({ signal }) => api.get("community/counts", countsSchema, { signal }),
  });
  const params = new URLSearchParams({ page: String(page) });
  if (status !== "all") params.set("status", status);
  if (tab === "candidate-skins") params.set("visibility", visibility);
  if (q) params.set("q", q);
  const query = params.toString();
  const list = useQuery({
    queryKey: keys.page("community", "list", tab, query),
    queryFn: ({ signal }) => api.get(`${tab}?${query}`, listSchema, { signal }),
    placeholderData: keepPreviousData,
  });

  const pending = counts.data ? Object.fromEntries(sections.map(section => [section, counts.data[section].pending])) as Record<Section, number> : undefined;
  const items = list.data?.items ?? [];
  const total = list.data?.total ?? 0;
  const pages = Math.max(1, Math.ceil(total / PAGE_SIZE));
  // A page that emptied after a review action (or a stale ?page= link) falls back to the last page that still has items.
  const lastPage = list.data && !list.isPlaceholderData && list.data.items.length === 0 && page > 1 ? pages : null;
  useEffect(() => {
    if (lastPage !== null) setSearch({ page: lastPage > 1 ? String(lastPage) : undefined });
  }, [lastPage, setSearch]);
  const target = (item: Item): Target => ({ section: tab, id: item.id, name: item.name, moderation: item.moderation, moderation_reason: item.moderation_reason, previous_moderation: item.previous_moderation, created_at: item.created_at, updated_at: item.updated_at });

  let body: ReactNode;
  if (list.isError && !list.data) {
    body = <ErrorState error={list.error} onRetry={() => list.refetch()} />;
  } else if (list.isPending) {
    body = <div className="grid grid-cols-[repeat(auto-fill,minmax(260px,1fr))] gap-3.5" role="status" aria-label="正在加载">
      {Array.from({ length: 6 }, (_, index) => (
        // biome-ignore lint/suspicious/noArrayIndexKey: placeholder cards have no identity
        <Skeleton key={index} className="h-[262px] rounded-[18px]" />
      ))}
    </div>;
  } else if (items.length === 0) {
    body = <div className="rounded-[18px] bg-panel-2 p-12 text-center text-muted" role="status">{q ? `没有匹配「${q}」的内容` : "这一栏已经清空了"}</div>;
  } else {
    body = <div className="grid grid-cols-[repeat(auto-fill,minmax(260px,1fr))] gap-3.5">
      {items.map(item => <ItemCard key={item.id} section={tab} item={item} canReview={canReview}
        onOpen={() => setOpen({ section: tab, id: item.id })}
        onApprove={() => approve(target(item))}
        onRemove={() => remove(target(item))}
        onRestore={() => restore(target(item))} />)}
    </div>;
  }

  return <>
    <PageIntro page="community" />
    <div className="mb-3.5 flex flex-wrap items-center gap-2.5">
      <SectionTabs value={tab} pending={pending} onChange={next => setSearch({ tab: next === "skins" ? undefined : next, page: undefined, visibility: undefined })} />
      <FilterChips label="审核状态" value={status} options={statusOptions} onChange={next => setSearch({ status: next === "pending" ? undefined : next, page: undefined })} />
      {tab === "candidate-skins" && <FilterChips label="可见性" value={visibility} options={visibilityOptions} onChange={next => setSearch({ visibility: next === "public" ? undefined : next, page: undefined })} />}
      <label className="relative flex h-9 w-full min-[820px]:ml-auto min-[820px]:w-[220px]">
        <span className="sr-only">搜索名称、作者</span>
        <Search size={16} aria-hidden="true" className="pointer-events-none absolute top-1/2 left-3 -translate-y-1/2 text-muted" />
        <input type="search" value={typed} maxLength={100} onChange={event => setTyped(event.target.value)} placeholder="搜索名称、作者…"
          className="h-9 w-full rounded-[10px] bg-panel pr-3 pl-9 text-[13.5px] text-ink ring-1 ring-hair-2 outline-none placeholder:text-muted focus:ring-[1.5px] focus:ring-accent" />
      </label>
    </div>
    {tab === "candidate-skins" && visibility === "private" && <p className="m-0 mb-3.5 text-[12.5px] text-muted">私有库只有作者本人可见，不计入待审核数。</p>}
    {counts.isError && <ErrorState error={counts.error} onRetry={() => counts.refetch()} className="mb-3.5" />}

    {body}

    {list.isError && list.data && <ErrorState error={list.error} onRetry={() => list.refetch()} className="mt-3.5" />}
    {total > PAGE_SIZE && <nav aria-label="分页" className="mt-4 flex flex-wrap items-center justify-center gap-3 text-[13px] text-muted">
      <Button size="sm" variant="outline" disabled={page <= 1} onClick={() => setSearch({ page: page > 2 ? String(page - 1) : undefined })}>上一页</Button>
      <span className="tabular-nums">第 {page} / {pages} 页 · 共 {formatNumber(total)} 项</span>
      <Button size="sm" variant="outline" disabled={!list.data?.has_more} onClick={() => setSearch({ page: String(page + 1) })}>下一页</Button>
    </nav>}

    <ContentDrawer target={open} onClose={() => { setOpen(null); if (focus) setSearch({ focus: undefined }); }}
      onApprove={approve} onRemove={remove} onRestore={restore} onCategory={setCategory} />
  </>;
}

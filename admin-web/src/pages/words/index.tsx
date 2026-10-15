import { useEffect, useMemo, useRef, useState } from "react";
import type { FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { format } from "date-fns";
import { errorMessage, isAPIError, useAPI } from "../../api/client";
import { keys } from "../../api/keys";
import { addSensitiveWordResultSchema, categoryLabels, displayPattern, levelLabels, parsePatternInput, sensitiveCategories, sensitiveWordsSchema } from "../../api/words";
import type { SensitiveCategory, SensitiveLevel, SensitiveWord } from "../../api/words";
import { MOBILE_QUERY, useMediaQuery } from "../../shell/media";
import { usePageSearch, useSetPageSearch } from "../../shell/page-search";
import { noPermissionHint, usePermissions } from "../../shell/permissions";
import { Button } from "../../ui/button";
import { cn } from "../../ui/cn";
import { useConfirm } from "../../ui/confirm";
import { CellText, DataTable } from "../../ui/data-table";
import type { BatchAction, Column } from "../../ui/data-table";
import { FilterChips } from "../../ui/filter-chips";
import { Pill } from "../../ui/pill";
import { StatGrid, StatTile } from "../../ui/stat-tile";
import { useToast } from "../../ui/toast";

type Filter = "all" | SensitiveCategory;

// The add bar lists 自定义 first because it is the default category.
const categoryOptions: readonly SensitiveCategory[] = ["custom", "ad", "vulgar", "abuse", "illegal"];

// Batch actions accept at most 100 ids per request, so larger selections go out in chunks.
const BATCH_LIMIT = 100;

function creatorLabel(createdBy: string): string {
  if (createdBy === "legacy-token") return "旧版密钥";
  const at = createdBy.indexOf("@");
  return at > 0 ? `@${createdBy.slice(0, at)}` : createdBy;
}

function createdLabel(value: string): string {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value;
  if (Date.now() - date.getTime() < 60_000) return "刚刚";
  return format(date, date.getFullYear() === new Date().getFullYear() ? "MM-dd" : "yyyy-MM-dd");
}

function chunks(ids: string[]): string[][] {
  const out: string[][] = [];
  for (let i = 0; i < ids.length; i += BATCH_LIMIT) out.push(ids.slice(i, i + BATCH_LIMIT));
  return out;
}

const selectClass = "h-[38px] rounded-[10px] bg-panel-2 px-2.5 text-[13.5px] text-ink outline-none focus:ring-[1.5px] focus:ring-accent disabled:opacity-45";

type RowHandlers = { toggleLevel: (word: SensitiveWord) => void; remove: (word: SensitiveWord) => void };

export default function WordsPage() {
  const api = useAPI();
  const client = useQueryClient();
  const toast = useToast();
  const confirm = useConfirm();
  const { can } = usePermissions();
  const canEdit = can("review_community");
  const { focus } = usePageSearch();
  const setSearch = useSetPageSearch();
  const mobile = useMediaQuery(MOBILE_QUERY);
  const [filter, setFilter] = useState<Filter>("all");
  const [text, setText] = useState("");
  const [category, setCategory] = useState<SensitiveCategory>("custom");
  const [level, setLevel] = useState<SensitiveLevel>("review");
  const input = useRef<HTMLInputElement>(null);

  const query = useQuery({
    queryKey: keys.page("words", "list"),
    queryFn: ({ signal }) => api.get("sensitive-words", sensitiveWordsSchema, { signal }),
  });
  const rows = query.data?.items;
  const maxHits = Math.max(query.data?.max_hits ?? 0, 1);
  const refresh = () => client.invalidateQueries({ queryKey: keys.page("words") });

  const add = useMutation({
    mutationFn: async (value: { pattern: string; category: SensitiveCategory; level: SensitiveLevel }) => addSensitiveWordResultSchema.parse(await api.action({ action: "add_sensitive_word", value })),
    onSettled: refresh,
  });

  const setLevels = async (targets: readonly SensitiveWord[], next: SensitiveLevel) => {
    try {
      for (const ids of chunks(targets.map(word => String(word.id)))) await api.action({ action: "set_sensitive_word_level", ids, value: next });
    } finally {
      await refresh();
    }
  };
  const removeWords = async (targets: readonly SensitiveWord[]) => {
    try {
      for (const ids of chunks(targets.map(word => String(word.id)))) await api.action({ action: "delete_sensitive_word", ids });
    } finally {
      await refresh();
    }
  };

  const submit = (event?: FormEvent) => {
    event?.preventDefault();
    const typed = text.trim();
    if (!typed) {
      toast("请先输入敏感词");
      input.current?.focus();
      return;
    }
    const { pattern } = parsePatternInput(typed);
    if (rows?.some(word => word.pattern === pattern)) {
      toast(`「${typed}」已在名单中`);
      return;
    }
    add.mutate({ pattern: typed, category, level }, {
      onSuccess: () => {
        setText(current => current.trim() === typed ? "" : current);
        toast(`已添加「${typed}」`);
      },
      onError: error => {
        if (isAPIError(error, "exists")) toast(`「${typed}」已在名单中`);
        else toast(`添加失败：${errorMessage(error)}`);
      },
    });
  };

  // Row buttons call through a ref so the column definitions stay referentially stable, as DataTable requires.
  const handlers = useRef<RowHandlers>({ toggleLevel: () => undefined, remove: () => undefined });
  useEffect(() => {
    handlers.current = {
      toggleLevel: word => {
        setLevels([word], word.level === "block" ? "review" : "block")
          .then(() => toast(`已更新「${displayPattern(word)}」的处理方式`))
          .catch(error => toast(`操作失败：${errorMessage(error)}`));
      },
      remove: word => {
        const shown = displayPattern(word);
        void confirm({ title: `删除「${shown}」？`, description: "删除后，提交中出现这个词将不再被标记。", okLabel: "删除" }).then(answer => {
          if (answer === null) return;
          return removeWords([word]).then(() => toast(`已删除「${shown}」`));
        }).catch(error => toast(`操作失败：${errorMessage(error)}`));
      },
    };
  });

  const counts = useMemo(() => {
    const byCategory: Partial<Record<SensitiveCategory, number>> = {};
    let block = 0;
    let hits = 0;
    for (const word of rows ?? []) {
      byCategory[word.category] = (byCategory[word.category] ?? 0) + 1;
      if (word.level === "block") block++;
      hits += word.hits_7d;
    }
    return { byCategory, block, review: (rows?.length ?? 0) - block, hits };
  }, [rows]);

  // Global search and notifications link here with ?focus=<word id>; the table then shows only that word until 显示全部.
  const focused = focus ? rows?.find(word => String(word.id) === focus) : undefined;
  const visible = useMemo(() => {
    if (focused) return [focused];
    return filter === "all" ? rows : rows?.filter(word => word.category === filter);
  }, [rows, filter, focused]);

  const columns = useMemo<Column<SensitiveWord>[]>(() => {
    const editTitle = canEdit ? undefined : noPermissionHint;
    return [
      {
        id: "word", header: "词 / 规则", width: "minmax(220px,1.6fr)",
        cell: word => <CellText title={displayPattern(word)} mono={word.is_regex}
          sub={<span title={word.created_by}>{word.is_regex ? "正则 · " : ""}{creatorLabel(word.created_by)} · {createdLabel(word.created_at)}</span>} />,
      },
      { id: "category", header: "分类", width: "100px", cell: word => categoryLabels[word.category] },
      { id: "level", header: "处理方式", width: "100px", cell: word => <Pill tone={word.level === "block" ? "bad" : "warn"}>{levelLabels[word.level]}</Pill> },
      {
        id: "hits", header: "近 7 天命中", width: "minmax(140px,1fr)",
        cell: word => <div className="flex items-center gap-2.5">
          <div className="h-1.5 min-w-0 flex-1 overflow-hidden rounded-full bg-panel-2" aria-hidden="true">
            <div className="h-full rounded-full bg-warn" style={{ width: `${Math.min(word.hits_7d / maxHits, 1) * 100}%` }} />
          </div>
          <span className="min-w-10 text-right text-[12.5px] text-body tabular-nums">{word.hits_7d.toLocaleString("zh-CN")}</span>
        </div>,
      },
      {
        id: "actions", header: "", width: "190px", align: "right",
        cell: word => <>
          <Button size="sm" variant="outline" disabled={!canEdit} title={editTitle} onClick={() => handlers.current.toggleLevel(word)}>{word.level === "block" ? "改为转人工" : "改为拦截"}</Button>
          <Button size="sm" variant="outline" disabled={!canEdit} title={editTitle} onClick={() => handlers.current.remove(word)}>删除</Button>
        </>,
      },
    ];
  }, [canEdit, maxHits]);

  const batchActions: BatchAction<SensitiveWord>[] = [
    {
      label: "改为直接拦截", disabled: !canEdit,
      onClick: async selected => { await setLevels(selected, "block"); toast(`${selected.length} 个词已改为直接拦截`); },
    },
    {
      label: "改为转人工", disabled: !canEdit,
      onClick: async selected => { await setLevels(selected, "review"); toast(`${selected.length} 个词已改为转人工`); },
    },
    {
      label: "删除", variant: "danger-outline", disabled: !canEdit,
      onClick: async selected => {
        if (await confirm({ title: `删除 ${selected.length} 个敏感词？`, description: "删除后，提交中出现这些词将不再被标记。", okLabel: "删除" }) === null) return false;
        await removeWords(selected);
        toast(`已删除 ${selected.length} 个词`);
      },
    },
  ];

  // On phones the chips get their own horizontally scrolling row, because the table toolbar leaves them too little width next to the search box.
  const chips = <FilterChips label="分类" value={filter} onChange={setFilter} className={cn(mobile && "flex-nowrap *:shrink-0")}
    options={[{ key: "all", label: "全部", count: rows?.length ?? 0 }, ...sensitiveCategories.map(key => ({ key, label: categoryLabels[key], count: counts.byCategory[key] ?? 0 }))]} />;
  // A link to a word that was deleted since says so instead of silently showing the whole list.
  const focusMissing = Boolean(focus) && rows !== undefined && !focused;
  const focusBar = (focused || focusMissing) && <div className="flex flex-wrap items-center gap-2 text-[13px] text-body">
    <span>{focused ? "仅显示搜索定位的词条" : "搜索定位的词条已不在名单中"}</span>
    <Button size="sm" variant="outline" onClick={() => setSearch({ focus: undefined })}>显示全部</Button>
  </div>;

  const stat = (value: number) => query.data ? value.toLocaleString("zh-CN") : "—";
  const editTitle = canEdit ? undefined : noPermissionHint;

  return <div>
    <form onSubmit={submit} className="flex flex-wrap gap-2 rounded-[18px] bg-panel p-3.5 ring-1 ring-hair" aria-label="添加敏感词">
      <input ref={input} value={text} onChange={event => setText(event.target.value)} maxLength={202} disabled={!canEdit} title={editTitle}
        placeholder="添加敏感词；以 / 开头写正则，例如 /加\s*v/" aria-label="敏感词或正则"
        className="h-[38px] min-w-0 flex-[1_1_260px] rounded-[10px] bg-panel-2 px-3 text-[13.5px] text-ink outline-none placeholder:text-muted focus:ring-[1.5px] focus:ring-accent disabled:opacity-45" />
      <select value={category} onChange={event => setCategory(event.target.value as SensitiveCategory)} aria-label="分类" disabled={!canEdit} className={selectClass}>
        {categoryOptions.map(key => <option key={key} value={key}>{categoryLabels[key]}</option>)}
      </select>
      <select value={level} onChange={event => setLevel(event.target.value as SensitiveLevel)} aria-label="处理方式" disabled={!canEdit} className={selectClass}>
        <option value="review">{levelLabels.review}</option>
        <option value="block">{levelLabels.block}</option>
      </select>
      <Button type="submit" variant="primary" size="lg" disabled={!canEdit || add.isPending} title={editTitle}
        className={cn(text.trim() === "" && "opacity-45")}>添加</Button>
    </form>
    <p className="mx-1 mt-2.5 mb-3.5 text-[12.5px] leading-relaxed text-muted">官网词库提交和社区内容进入审核队列前，会先和这份名单比对。「直接拦截」的提交会被自动驳回，「转人工」的会在审核页标黄提示。</p>

    <StatGrid className="mb-3.5">
      <StatTile label="词条总数" value={stat(rows?.length ?? 0)} />
      <StatTile label="直接拦截" value={stat(counts.block)} />
      <StatTile label="转人工" value={stat(counts.review)} />
      <StatTile label="近 7 天命中" value={stat(counts.hits)} />
    </StatGrid>

    {mobile && !focusBar && <div className="mb-3 overflow-x-auto pb-1">{chips}</div>}

    <DataTable
      ariaLabel="敏感词列表"
      data={visible}
      loading={query.isPending}
      error={query.error}
      onRetry={() => void query.refetch()}
      columns={columns}
      getRowId={word => String(word.id)}
      selectable
      batchActions={batchActions}
      toolbar={focusBar || (mobile ? undefined : chips)}
      searchText={word => `${displayPattern(word)} ${categoryLabels[word.category]} ${levelLabels[word.level]} ${word.created_by}`}
      emptyText={rows?.length === 0 ? "名单还是空的，在上方添加第一个敏感词。" : "这一栏是空的"}
      minWidth="790px"
    />
  </div>;
}

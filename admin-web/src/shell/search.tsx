import { useMemo, useRef, useState } from "react";
import { Command } from "cmdk";
import { useQuery } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { useHotkeys } from "react-hotkeys-hook";
import { Search } from "lucide-react";
import { errorMessage, useAPI } from "../api/client";
import { keys } from "../api/keys";
import { searchSchema } from "../api/shell";
import { navItems, pageForTarget } from "../nav";
import { overlayOpen } from "../ui/overlay";
import { useDebounced } from "../hooks/use-debounced";
import { navAllowed, usePermissions } from "./permissions";

type Result = { key: string; title: string; where: string; path: string; focus?: string };

// GlobalSearch matches page names locally and asks /api/search for users, community items, PRs, issues, releases, crash groups, sensitive words and notices. "/" focuses it.
export function GlobalSearch() {
  const api = useAPI();
  const navigate = useNavigate();
  const permissions = usePermissions();
  const input = useRef<HTMLInputElement>(null);
  const [query, setQuery] = useState("");
  const [focused, setFocused] = useState(false);
  const q = query.trim();
  const debounced = useDebounced(q, 200);
  useHotkeys("/", event => {
    // Below 900px the header hides the search box (display:none, so no offsetParent); the key then types normally instead of being swallowed.
    if (overlayOpen() || !input.current || input.current.offsetParent === null) return;
    event.preventDefault();
    input.current?.focus();
  }, { useKey: true });

  const server = useQuery({
    queryKey: keys.search(debounced),
    queryFn: ({ signal }) => api.get(`search?q=${encodeURIComponent(debounced)}`, searchSchema, { signal }),
    enabled: debounced.length > 0,
    staleTime: 30_000,
  });

  const results = useMemo<Result[]>(() => {
    if (!q) return [];
    const lower = q.toLowerCase();
    const pages = navItems.filter(item => navAllowed(permissions, item)).filter(item => item.label.toLowerCase().includes(lower) || item.key.includes(lower)).map(item => ({ key: `page:${item.key}`, title: item.label, where: "页面", path: item.path }));
    const items = debounced === q && server.data ? server.data.items.map(item => ({ key: `${item.kind}:${item.id}`, title: item.title, where: item.where, path: pageForTarget(item.target), focus: item.id })) : [];
    return [...pages, ...items].slice(0, 8);
  }, [q, debounced, server.data, permissions]);

  const pick = (result: Result) => {
    setQuery("");
    input.current?.blur();
    void navigate({ to: result.path, search: result.focus ? { focus: result.focus } : {} });
  };

  const open = focused && q.length > 0;
  return <Command shouldFilter={false} label="全局搜索" className="relative w-[260px]">
    <div className="relative">
      <Search size={16} aria-hidden="true" className="pointer-events-none absolute top-1/2 left-3 -translate-y-1/2 text-muted" />
      <Command.Input ref={input} value={query} onValueChange={setQuery} placeholder="搜索页面、PR、Issue、用户…"
        onFocus={() => setFocused(true)} onBlur={() => setFocused(false)}
        onKeyDown={event => { if (event.key === "Escape") { setQuery(""); event.currentTarget.blur(); } }}
        className="h-9 w-full rounded-[10px] bg-panel pr-9 pl-9 text-[13.5px] text-ink outline-none inset-ring inset-ring-hair-2 placeholder:text-muted focus:inset-ring-[1.5px] focus:inset-ring-accent" />
      <kbd className="pointer-events-none absolute top-1/2 right-2.5 -translate-y-1/2 rounded-[5px] bg-panel-2 px-1.5 font-mono text-[11px] text-muted">/</kbd>
    </div>
    {open && <Command.List onMouseDown={event => event.preventDefault()} className="absolute top-[42px] right-0 left-0 z-31 max-h-[360px] animate-pop-in overflow-y-auto rounded-[14px] bg-panel p-1.5 shadow-pop">
      {results.length === 0 && !(server.isFetching || debounced !== q) && <Command.Empty className="px-2.5 py-3 text-[13px] text-muted">没有找到相关结果</Command.Empty>}
      {results.map(result => <Command.Item key={result.key} value={result.key} onSelect={() => pick(result)}
        className="flex cursor-pointer items-center gap-3 rounded-[9px] px-2.5 py-[9px] text-[13.5px] data-[selected=true]:bg-panel-2">
        <span className="min-w-0 flex-1 truncate text-ink">{result.title}</span>
        <span className="shrink-0 text-xs text-muted">{result.where}</span>
      </Command.Item>)}
      {(server.isFetching || debounced !== q) && <div className="px-2.5 py-2 text-xs text-muted" role="status">正在搜索…</div>}
      {server.isError && debounced === q && <div className="px-2.5 py-2 text-xs text-bad" role="alert">{errorMessage(server.error)}</div>}
    </Command.List>}
  </Command>;
}

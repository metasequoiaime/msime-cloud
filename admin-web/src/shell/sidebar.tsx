import { Link } from "@tanstack/react-router";
import { useAuth } from "../auth";
import { roleLabel } from "../api/shell";
import { meItem, navGroups } from "../nav";
import { cn } from "../ui/cn";
import logo from "../assets/msime.png";
import { NavIcon } from "./nav-icon";
import { navAllowed, usePermissions } from "./permissions";
import { useShell } from "./shell-data";
import { useCommunityCounts } from "../hooks/use-community-counts";

const statusText = { ok: "后端运行正常", degraded: "部分服务降级", down: "后端服务异常", unknown: "状态未知" } as const;
const statusDot = { ok: "bg-accent ring-accent-soft", degraded: "bg-warn ring-warn-soft", down: "bg-bad ring-bad-soft", unknown: "bg-muted ring-panel-2" } as const;

// Sidebar is the grouped navigation. expanded is false only for the collapsed desktop rail; the mobile drawer always renders expanded content.
export function Sidebar({ expanded, mobile, mobileOpen, onToggle, onNavigate }: { expanded: boolean; mobile: boolean; mobileOpen: boolean; onToggle: () => void; onNavigate: () => void }) {
  const { session } = useAuth();
  const shell = useShell();
  const community = useCommunityCounts();
  const permissions = usePermissions();
  const data = shell.data;
  const version = data?.version ?? session?.version;
  const email = data?.me.email ?? session?.email ?? "";
  const name = data?.me.name || email.split("@")[0] || "管理员";
  const role = data ? roleLabel(data.me.role) : "";
  // The closed mobile drawer is off-screen; inert keeps its links out of the tab order and the accessibility tree.
  return <aside id="admin-navigation" aria-label="后台导航" inert={mobile && !mobileOpen}
    className={cn("z-50 flex h-screen shrink-0 flex-col bg-panel shadow-[inset_-1px_0_0_var(--hair)] transition-[width,translate] duration-200",
      mobile ? "fixed inset-y-0 left-0 w-58" : cn("sticky top-0", expanded ? "w-44" : "w-16"),
      mobile && !mobileOpen && "-translate-x-[105%]")}>
    <Link to="/" onClick={onNavigate} className="flex h-16 shrink-0 items-center gap-2.5 border-b border-hair px-[18px] text-ink no-underline hover:text-ink">
      <span className="grid h-8 w-8 shrink-0 place-items-center rounded-full bg-[#1E1F1C]"><img src={logo} alt="" className="h-[22px] w-[22px] object-contain" /></span>
      {expanded && <span className="min-w-0">
        <span className="flex items-center gap-1.5"><span className="text-[15px] font-bold">水杉</span>{version && <span className="inline-flex h-[18px] items-center rounded-[5px] bg-accent-soft px-1.5 font-mono text-[10.5px] text-accent-ink" title={`服务版本 ${version}`}>v{version}</span>}</span>
        <span className="block text-[11.5px] text-muted">管理后台</span>
      </span>}
    </Link>
    <nav className="min-h-0 flex-1 overflow-y-auto px-2.5 py-3">
      {navGroups.map(group => ({ ...group, items: group.items.filter(item => navAllowed(permissions, item)) })).filter(group => group.items.length > 0).map(group => <div key={group.title} className="mb-3.5">
        {expanded && <div className="px-2.5 py-1.5 text-[11.5px] font-semibold text-muted">{group.title}</div>}
        {group.items.map(item => {
          const badge = item.communityBadge ? community.data?.[item.communityBadge].pending ?? 0 : item.badge && data ? data.pending[item.badge] : 0;
          return <Link key={item.key} to={item.path} title={item.label} onClick={onNavigate} activeOptions={{ exact: item.path === "/" }}
            className="group relative flex h-[38px] items-center gap-2.5 rounded-[10px] px-2.5 text-body no-underline transition hover:bg-panel-2 hover:text-body data-[status=active]:bg-accent-soft data-[status=active]:font-semibold data-[status=active]:text-accent-ink">
            <NavIcon d={item.icon} className="text-muted group-data-[status=active]:text-accent-ink" />
            {expanded && <span className="min-w-0 flex-1 truncate text-[13.5px]">{item.label}</span>}
            {badge > 0 && <span className={cn("inline-flex h-5 min-w-5 items-center justify-center rounded-full bg-accent px-1.5 text-[11.5px] font-semibold text-btn-fg tabular-nums", !expanded && "absolute -top-0.5 right-0.5 h-4 min-w-4 px-1 text-[10px]")}>{badge > 99 ? "99+" : badge}<span className="sr-only"> 项待处理</span></span>}
          </Link>;
        })}
      </div>)}
    </nav>
    <div className="shrink-0 border-t border-hair p-2.5">
      {expanded && <Link to="/status" onClick={onNavigate} className="mb-1 block rounded-[10px] px-2.5 py-2 no-underline hover:bg-panel-2">
        <span className="flex items-center gap-2 text-[13px] text-body"><span className={cn("h-[7px] w-[7px] rounded-full ring-[3px]", data ? statusDot[data.status] : "bg-muted ring-panel-2")} />{data ? statusText[data.status] : "状态未知"}</span>
        <span className="block text-[11.5px] leading-[1.7] text-muted">管理操作已启用审计</span>
      </Link>}
      <button type="button" onClick={onToggle} title={mobile ? "关闭导航" : expanded ? "收起侧栏" : "展开侧栏"} aria-label={mobile ? "关闭导航" : expanded ? "收起侧栏" : "展开侧栏"} aria-expanded={mobile ? mobileOpen : expanded}
        className="flex h-9 w-full items-center gap-2.5 rounded-[10px] px-2.5 text-[13px] text-muted transition hover:bg-panel-2 hover:text-ink">
        <svg aria-hidden="true" viewBox="0 0 24 24" width="22" height="22" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round" className={cn("shrink-0 transition-transform", !expanded && "-scale-x-100")}><rect x="3.5" y="4.5" width="17" height="15" rx="2.5" /><path d="M9 4.5v15M15.5 10l-2 2 2 2" /></svg>
        {expanded && <span>收起侧栏</span>}
      </button>
      <Link to={meItem.path} onClick={onNavigate} title="个人中心" className="mt-1 flex items-center gap-2.5 rounded-[10px] px-1.5 py-1.5 no-underline transition hover:bg-panel-2 data-[status=active]:bg-accent-soft">
        <span className="grid h-8 w-8 shrink-0 place-items-center rounded-full bg-accent-soft text-[13px] font-bold text-accent-ink">{(name[0] ?? "?").toUpperCase()}</span>
        {expanded && <span className="min-w-0">
          <span className="block truncate text-[13px] font-semibold text-ink">{name}</span>
          <span className="block truncate text-xs text-muted">{role || email}</span>
        </span>}
      </Link>
    </div>
  </aside>;
}

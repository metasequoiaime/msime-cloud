import { Link, Outlet, createRootRoute, createRoute, createRouter, lazyRouteComponent, redirect } from "@tanstack/react-router";
import { Login, useAuth } from "../auth";
import { AppShell } from "../shell/app-shell";
import { validatePageSearch } from "../shell/page-search";
import { Empty } from "../ui/states";
import { communityPathForSearch } from "../nav";

function Root() {
  const { authenticated, loading } = useAuth();
  if (loading) return <section className="grid min-h-screen place-items-center bg-bg p-6"><div className="rounded-[18px] bg-panel p-8 text-sm text-muted ring-1 ring-hair" role="status">正在恢复登录状态…</div></section>;
  if (!authenticated) return <Login />;
  return <AppShell />;
}

function NotFound() {
  return <Empty title="页面不存在"><Link to="/">返回数据概览</Link></Empty>;
}

const rootRoute = createRootRoute({ component: Root, notFoundComponent: NotFound });

// Each page is its own lazy chunk; page units own src/pages/<page>/ and must keep a default export in index.tsx.
const page = <P extends string>(path: P, load: () => Promise<{ default: () => React.ReactNode }>) =>
  createRoute({ getParentRoute: () => rootRoute, path, validateSearch: validatePageSearch, component: lazyRouteComponent(load) });

const overviewRoute = page("/", () => import("../pages/overview"));
const dictionaryRoute = page("/dictionary", () => import("../pages/dictionary"));
const dictprRoute = page("/dictpr", () => import("../pages/dictpr"));
const skinsRoute = page("/skins", () => import("../pages/skins"));
const candidateSkinsRoute = page("/candidate-skins", () => import("../pages/candidate-skins"));
const pluginsRoute = page("/plugins", () => import("../pages/plugins"));
const dictionariesRoute = page("/dictionaries", () => import("../pages/dictionaries"));
const repliesRoute = page("/replies", () => import("../pages/replies"));
const phrasesRoute = page("/phrases", () => import("../pages/phrases"));
const issuesRoute = page("/issues", () => import("../pages/issues"));
const feedbackRoute = page("/feedback", () => import("../pages/feedback"));
const wordsRoute = page("/words", () => import("../pages/words"));
const usersRoute = page("/users", () => import("../pages/users"));
const downloadsRoute = page("/downloads", () => import("../pages/downloads"));
const noticeRoute = page("/notice", () => import("../pages/notice"));
const releaseRoute = page("/release", () => import("../pages/release"));
const cloudRoute = page("/cloud", () => import("../pages/cloud"));
const crashRoute = page("/crash", () => import("../pages/crash"));
const statusRoute = page("/status", () => import("../pages/status"));
const logsRoute = page("/logs", () => import("../pages/logs"));
const permRoute = page("/perm", () => import("../pages/perm"));
const meRoute = page("/me", () => import("../pages/me"));

// 旧后台路径保留筛选及定位参数，社区旧入口按内容类型迁移。
const moved = <P extends string>(path: P, to: "/perm" | "/status" | "/crash" | "/downloads") =>
  createRoute({ getParentRoute: () => rootRoute, path, component: Outlet, beforeLoad: ({ location }) => {
    throw redirect({ to, search: validatePageSearch(location.search as Record<string, unknown>) as never, replace: true });
  } });

const communityRoute = createRoute({ getParentRoute: () => rootRoute, path: "/community", component: Outlet, beforeLoad: ({ location }) => {
  const search = validatePageSearch(location.search as Record<string, unknown>);
  throw redirect({ to: communityPathForSearch(search), search: validatePageSearch({ ...search, tab: undefined }) as never, replace: true });
} });

const routeTree = rootRoute.addChildren([
  overviewRoute, dictionaryRoute, dictprRoute, communityRoute, skinsRoute, candidateSkinsRoute, pluginsRoute, dictionariesRoute, repliesRoute, phrasesRoute, issuesRoute, feedbackRoute, wordsRoute, usersRoute, downloadsRoute, noticeRoute, releaseRoute, cloudRoute, crashRoute, statusRoute, logsRoute, permRoute, meRoute,
  moved("/admins", "/perm"), moved("/audit", "/perm"), moved("/system", "/status"), moved("/crashes", "/crash"), moved("/site-settings", "/downloads"),
]);

export const router = createRouter({ routeTree, defaultPreload: "intent", scrollRestoration: true });

declare module "@tanstack/react-router" {
  interface Register { router: typeof router }
}

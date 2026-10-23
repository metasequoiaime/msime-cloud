import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState } from "react";
import type { ReactNode } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { z } from "zod";
import { ArrowRight } from "lucide-react";
import { APICredentialsContext, APIError, errorMessage, requestAPI } from "./api/client";
import logo from "./assets/msime.png";
import { useFlushToast, useResetToast } from "./ui/toast";

const sessionSchema = z.object({ version: z.string().optional(), authenticated: z.boolean(), email: z.string(), google_enabled: z.boolean(), token_enabled: z.boolean(), can_manage_admins: z.boolean().default(false) });
export type Session = z.infer<typeof sessionSchema>;
type Auth = {
  authenticated: boolean;
  loading: boolean;
  session: Session | null;
  error: string;
  logout: () => Promise<void>;
  login: (token: string) => Promise<void>;
  reload: () => void;
};
const AuthContext = createContext<Auth | null>(null);

export function AuthProvider({ children }: { children: ReactNode }) {
  const [identity, setIdentity] = useState({ token: "" });
  const currentIdentity = useRef(identity);
  const token = identity.token;
  const [session, setSession] = useState<Session | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const client = useQueryClient();
  useEffect(() => {
    const controller = new AbortController();
    setLoading(true);
    setError("");
    requestAPI("auth/session", sessionSchema, { signal: controller.signal })
      .then(value => { if (!controller.signal.aborted) setSession(value); })
      .catch(err => { if (!controller.signal.aborted) setError(errorMessage(err)); })
      .finally(() => { if (!controller.signal.aborted) setLoading(false); });
    return () => controller.abort();
  }, []);
  const flushToast = useFlushToast();
  const resetToast = useResetToast();
  const replaceToken = useCallback((value: string) => {
    // 同一密钥重新登录也属于新身份；立即更新引用，避免旧响应抢在重绘前清掉新登录。
    const next = { token: value };
    currentIdentity.current = next;
    setIdentity(next);
  }, []);
  const clear = useCallback(() => {
    replaceToken("");
    setSession(previous => previous ? { ...previous, authenticated: false, email: "" } : previous);
    client.clear();
  }, [client, replaceToken]);
  const onUnauthorized = useCallback(() => {
    if (currentIdentity.current !== identity) return;
    resetToast();
    clear();
    setError("登录已失效，请重新登录。");
  }, [identity, clear, resetToast]);
  const logout = useCallback(async () => {
    setError("");
    // 先用当前身份提交仍在 4 秒撤销窗口内的合并或驳回，否则退出后会因 401 丢失。
    resetToast({ preservePending: true });
    await flushToast();
    if (currentIdentity.current !== identity) return;
    try {
      await requestAPI("auth/logout", z.unknown(), { method: "POST", body: {}, token });
      if (currentIdentity.current === identity) clear();
    } catch (err) {
      if (currentIdentity.current === identity) setError(errorMessage(err));
    }
  }, [token, identity, clear, flushToast, resetToast]);
  // The legacy admin token is verified through /api/auth/session, which reports authenticated for a valid bearer token.
  const login = useCallback(async (value: string) => {
    const loginIdentity = currentIdentity.current;
    const result = await requestAPI("auth/session", sessionSchema, { token: value });
    if (!result.authenticated) throw new APIError(401, "unauthorized");
    // 切换身份不应阻塞登录；先清除旧提示并发出尚未提交的延迟操作。
    resetToast({ preservePending: true });
    void flushToast();
    if (currentIdentity.current !== loginIdentity) return;
    client.clear();
    replaceToken(value);
    setError("");
    setSession(result);
  }, [client, flushToast, replaceToken, resetToast]);
  const credentials = useMemo(() => ({ token, onUnauthorized }), [token, onUnauthorized]);
  const value = useMemo<Auth>(() => ({ authenticated: session?.authenticated ?? false, loading, session, error, logout, login, reload: () => window.location.reload() }), [session, loading, error, logout, login]);
  return <AuthContext.Provider value={value}><APICredentialsContext.Provider value={credentials}>{children}</APICredentialsContext.Provider></AuthContext.Provider>;
}

export function useAuth() {
  const value = useContext(AuthContext);
  if (!value) throw new Error("AuthProvider required");
  return value;
}

export function Login() {
  const { login, session, error: authError, reload } = useAuth();
  const [value, setValue] = useState("");
  const [pending, setPending] = useState(false);
  const [error, setError] = useState("");
  const denied = new URLSearchParams(window.location.search).has("login_error");
  return <section className="grid min-h-screen bg-bg lg:grid-cols-2">
    <div className="mx-auto flex w-full max-w-[460px] flex-col justify-center px-7 py-12 sm:px-12">
      <img className="mb-7 h-16 w-16 object-contain" src={logo} alt="" />
      <p className="font-mono text-[10px] font-medium tracking-[2px] text-muted">MSIME / ADMIN</p>
      <h1 className="my-3 text-3xl font-bold tracking-[-1px] text-ink">水杉管理后台</h1>
      <p className="text-sm leading-7 text-muted">审核社区内容、跟进问题反馈、管理发布与云服务。</p>
      {session?.google_enabled && <div className="my-8">
        <a className="flex items-center justify-center gap-3 rounded-[11px] bg-panel px-3 py-3 text-sm font-medium text-ink no-underline ring-1 ring-hair-2 transition hover:bg-panel-2 hover:text-ink" href="/api/auth/google/start">
          <svg width="20" height="20" viewBox="0 0 24 24" aria-hidden="true"><path fill="#4285F4" d="M21.6 12.23c0-.71-.06-1.39-.18-2.05H12v3.88h5.38a4.6 4.6 0 0 1-2 3.02v2.51h3.24c1.9-1.74 2.98-4.31 2.98-7.36Z" /><path fill="#34A853" d="M12 22c2.7 0 4.96-.9 6.62-2.41l-3.24-2.51c-.9.6-2.06.96-3.38.96-2.6 0-4.8-1.76-5.59-4.12H3.07v2.59A10 10 0 0 0 12 22Z" /><path fill="#FBBC05" d="M6.41 13.92a6 6 0 0 1 0-3.84V7.49H3.07a10 10 0 0 0 0 9.02l3.34-2.59A5.99 5.99 0 0 1 12 5.96Z" /><path fill="#EA4335" d="M12 5.96c1.47 0 2.79.5 3.82 1.49l2.87-2.87A9.6 9.6 0 0 0 12 2a10 10 0 0 0-8.93 5.49l3.34 2.59A5.99 5.99 0 0 1 12 5.96Z" /></svg>
          使用 Google 账号登录
        </a>
        <p className="mt-3 text-xs leading-6 text-muted">仅限已授权的管理员账号。登录会话有效期为 8 小时。</p>
      </div>}
      {session?.token_enabled && <details className="mt-8" open={!session.google_enabled}>
        <summary className="cursor-pointer text-xs text-muted">管理员密钥登录</summary>
        <form className="mt-4 grid gap-3" onSubmit={async event => {
          event.preventDefault();
          setPending(true);
          setError("");
          try { await login(value.trim()); setValue(""); } catch (e) { setError(errorMessage(e)); } finally { setPending(false); }
        }}>
          <label className="text-xs text-body" htmlFor="token">管理员密钥</label>
          <input className="h-10 rounded-[10px] bg-panel-2 px-3 text-sm text-ink outline-none focus:ring-[1.5px] focus:ring-accent" id="token" type="password" required autoComplete="off" value={value} onChange={event => setValue(event.target.value)} placeholder="输入管理员密钥" />
          <button className="flex h-10 items-center justify-between rounded-[10px] bg-btn px-4 text-sm font-semibold text-btn-fg transition hover:opacity-90 disabled:opacity-50" type="submit" disabled={pending}>{pending ? "正在验证…" : "进入控制台"}<ArrowRight size={18} aria-hidden="true" /></button>
        </form>
        <p className="mt-3 text-xs leading-6 text-muted">密钥仅保留在当前页面内存中，刷新后需重新输入。</p>
      </details>}
      {(denied || error || authError) && <p className="mt-5 text-sm text-bad" role="alert">{denied ? "Google 登录未完成或账号未获授权，请使用管理员账号重试。" : error || authError}</p>}
      {!session && <button className="mt-5 h-9 rounded-[10px] px-3 text-sm text-ink ring-1 ring-hair-2" type="button" onClick={reload}>重新加载登录方式</button>}
    </div>
    <div className="relative hidden overflow-hidden bg-accent-soft p-16 lg:flex lg:flex-col lg:justify-end">
      <div className="absolute left-12 top-[-250px] h-[800px] w-[800px] rounded-full ring-1 ring-accent-ring" />
      <div className="absolute left-32 top-[-170px] h-[640px] w-[640px] rounded-full ring-1 ring-accent-ring" />
      <p className="relative text-5xl font-light leading-[1.5] text-ink">让每一次输入<br />都更自然。</p>
      <span className="relative mt-4 text-xs tracking-[3px] text-muted">水杉输入法 · MSIME</span>
    </div>
  </section>;
}

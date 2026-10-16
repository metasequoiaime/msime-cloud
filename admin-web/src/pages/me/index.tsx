import { formatNumber } from "../../utils/number";
import { useState } from "react";
import { useIsMutating, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { differenceInCalendarDays, format } from "date-fns";
import { Copy } from "lucide-react";
import { useAuth } from "../../auth";
import { errorMessage, useAPI } from "../../api/client";
import { keys } from "../../api/keys";
import { issuedTokenSchema, meActionSchema, meSchema, prefRows } from "../../api/me";
import type { Me, MeAction, MeSession } from "../../api/me";
import { describeAudit, initial } from "../../api/perm";
import { roleLabel } from "../../api/shell";
import { PageIntro } from "../../shell/page-intro";
import { Button } from "../../ui/button";
import { Banner } from "../../ui/card";
import { cn } from "../../ui/cn";
import { useConfirm } from "../../ui/confirm";
import { Pill } from "../../ui/pill";
import { StatGrid, StatTile } from "../../ui/stat-tile";
import { Empty, ErrorState, Skeleton, SkeletonRows } from "../../ui/states";
import { Switch } from "../../ui/switch";
import { useToast } from "../../ui/toast";
import { relativeTime } from "../../utils/time";
import { Avatar, SectionCard, SectionRow } from "./section";

const meKey = keys.page("me");

export default function MePage() {
  const { logout } = useAuth();
  const confirm = useConfirm();
  const api = useAPI();
  const query = useQuery({ queryKey: meKey, queryFn: ({ signal }) => api.get("me", meSchema, { signal }) });
  const me = query.data;
  const legacy = me?.via === "legacy";
  return <>
    <PageIntro page="me" />
    <div className="grid gap-3.5">
      {query.isPending ? <Skeleton className="h-[110px] rounded-[18px]" /> : query.isError ? <ErrorState error={query.error} onRetry={() => query.refetch()} /> : me && <Profile me={me} />}
      {me && <Stats me={me} />}
      {legacy && <Banner tone="warn">当前使用旧版管理令牌登录：它不属于任何管理员，没有通知偏好、登录设备和个人访问令牌。请改用 Google 账号登录。</Banner>}
      <div className="grid items-start gap-3.5 min-[1100px]:grid-cols-2">
        <div className="grid min-w-0 gap-3.5">
          {me ? !legacy && <Preferences me={me} /> : query.isPending && <SkeletonCard title="通知偏好" />}
          {me ? <Security me={me} /> : query.isPending && <SkeletonCard title="安全" />}
        </div>
        <div className="grid min-w-0 gap-3.5">
          {me ? !legacy && <Sessions me={me} /> : query.isPending && <SkeletonCard title="登录设备" />}
          {me ? <Recent me={me} /> : query.isPending && <SkeletonCard title="我的最近操作" />}
          <Button variant="danger-outline" className="h-10 w-full rounded-xl text-sm" onClick={async () => {
            if (await confirm({ title: "退出登录？", description: "当前浏览器的管理会话会被注销，需要重新登录。", okLabel: "退出" }) !== null) await logout();
          }}>退出登录</Button>
        </div>
      </div>
    </div>
  </>;
}

function SkeletonCard({ title }: { title: string }) {
  return <SectionCard title={title}><SkeletonRows rows={3} /></SectionCard>;
}

function useMeAction() {
  const api = useAPI();
  return (body: MeAction) => api.post("me", meActionSchema, body);
}

function Profile({ me }: { me: Me }) {
  const toast = useToast();
  const display = me.name || me.email.split("@")[0] || "管理员";
  const meta = me.via === "legacy"
    ? ["旧版管理令牌"]
    : [me.email, me.via === "token" ? "个人访问令牌访问" : "Google 账号登录", me.joined_at ? `${format(new Date(me.joined_at), "yyyy-MM")} 加入` : me.owner ? "部署配置的所有者" : ""];
  return <div className="flex flex-wrap items-center gap-[18px] rounded-[18px] bg-panel px-6 py-[22px] ring-1 ring-hair">
    <Avatar letter={initial(display)} size="lg" />
    <div className="min-w-[200px] flex-1">
      <div className="flex flex-wrap items-center gap-2">
        <span className="text-xl font-bold text-ink">{display}</span>
        <Pill tone="accent">{roleLabel(me.role)}</Pill>
        {me.owner && <Pill tone="info">所有者</Pill>}
      </div>
      <p className="m-0 mt-1 break-all text-[13px] text-muted">{meta.filter(Boolean).join(" · ")}</p>
    </div>
    <Button variant="outline" onClick={() => toast("资料同步自 Google 账号，请在 Google 账号中修改头像和昵称")}>编辑资料</Button>
  </div>;
}

function Stats({ me }: { me: Me }) {
  const hours = me.stats.avg_handle_hours;
  return <StatGrid>
    <StatTile label="本月审核词库 PR" value={formatNumber(me.stats.dict_prs_month)} />
    <StatTile label="本月审核社区内容" value={formatNumber(me.stats.community_month)} />
    <StatTile label="本月分诊 Issue" value={formatNumber(me.stats.issues_month)} />
    <StatTile label="平均处理时长" value={hours === null ? "—" : `${hours < 10 ? hours.toFixed(1) : Math.round(hours)} 小时`} sub="本月社区内容，从提交到处理" />
  </StatGrid>;
}

function Preferences({ me }: { me: Me }) {
  const client = useQueryClient();
  const toast = useToast();
  const run = useMeAction();
  const mutationKey = keys.page("me", "preferences", me.email);
  const pending = useIsMutating({ mutationKey, exact: true }) > 0;
  const save = useMutation({
    mutationKey,
    mutationFn: (v: { key: string; value: boolean }) => run({ action: "set_pref", ...v }),
    onMutate: async v => {
      const query = client.getQueryCache().find({ queryKey: meKey, exact: true });
      await client.cancelQueries({ queryKey: meKey });
      if (!query || client.getQueryCache().find({ queryKey: meKey, exact: true }) !== query) return;
      const previous = client.getQueryData<Me>(meKey);
      client.setQueryData<Me>(meKey, old => old && { ...old, prefs: { ...old.prefs, [v.key]: v.value } });
      return { previous, query };
    },
    onError: (error, _v, context) => {
      // 登录切换会替换查询对象；旧 mutation 只能回滚自己所属的缓存，不能恢复旧身份资料。
      if (context?.previous && client.getQueryCache().find({ queryKey: meKey, exact: true }) === context.query) client.setQueryData(meKey, context.previous);
      toast(`操作失败：${errorMessage(error)}`);
    },
    // The notify_* preferences filter the bell's list and unread count on the server, so both refresh with the profile.
    onSettled: () => Promise.all([client.invalidateQueries({ queryKey: meKey }), client.invalidateQueries({ queryKey: keys.shell }), client.invalidateQueries({ queryKey: keys.notifications })]),
  });
  return <SectionCard title="通知偏好">
    {prefRows.map(row => (
      // biome-ignore lint/a11y/noLabelWithoutControl: Switch renders a Radix button, which is labelable, so clicking anywhere on the row toggles it
      <label key={row.key} className={cn("flex items-center gap-3 border-b border-hair px-5 py-[13px] last:border-b-0", row.available ? "cursor-pointer hover:bg-panel-2" : "cursor-not-allowed")}>
      <span className="min-w-0 flex-1">
        <span className="flex flex-wrap items-center gap-2 text-ink">{row.name}{!row.available && <Pill tone="mute">未接入</Pill>}</span>
        <span className="mt-0.5 block text-xs text-muted">{row.available ? row.description : `${row.description}；服务端尚未实现发送，暂不可开启`}</span>
      </span>
      <Switch label={row.name} checked={row.available && (me.prefs[row.key] ?? false)} disabled={!row.available || pending} onCheckedChange={value => {
        // 共享资料缓存只允许一项偏好保存，切换页面后返回也等待原写入与刷新结束。
        if (!client.isMutating({ mutationKey, exact: true })) save.mutate({ key: row.key, value });
      }} />
    </label>))}
  </SectionCard>;
}

function Security({ me }: { me: Me }) {
  const api = useAPI();
  const client = useQueryClient();
  const toast = useToast();
  const confirm = useConfirm();
  const [issued, setIssued] = useState<string | null>(null);
  const regenerate = useMutation({
    mutationFn: () => api.post("me", issuedTokenSchema, { action: "regenerate_token" } satisfies MeAction),
    onSuccess: result => {
      setIssued(result.token);
      toast(me.token ? "新令牌已生成，旧令牌立即失效" : "新令牌已生成");
      return client.invalidateQueries({ queryKey: meKey });
    },
    onError: error => toast(`操作失败：${errorMessage(error)}`),
  });
  const legacy = me.via === "legacy";
  const days = me.token ? Math.max(0, differenceInCalendarDays(new Date(me.token.expires_at), new Date())) : 0;
  const canRegenerate = me.via === "session";
  const expiry = days > 0 ? `${days} 天后过期` : "今天过期";
  // How this request authenticated: only a Google session is covered by Google two-step verification.
  const login = legacy
    ? { name: "旧版管理令牌", detail: "部署配置的共享令牌，不能管理权限", tone: "warn", pill: "共享令牌" } as const
    : me.via === "token"
      ? { name: "个人访问令牌", detail: `本次通过 Bearer 令牌访问，不受 Google 两步验证保护${me.token ? ` · ${expiry}` : ""}`, tone: "warn", pill: "令牌访问" } as const
      : { name: "Google 账号登录", detail: "两步验证与通行密钥由 Google 账号控制", tone: "accent", pill: "已开启" } as const;
  return <SectionCard title="安全">
    <SectionRow>
      <div className="min-w-0 flex-1">
        <div className="text-ink">{login.name}</div>
        <div className="mt-0.5 text-xs text-muted">{login.detail}</div>
      </div>
      <Pill tone={login.tone}>{login.pill}</Pill>
    </SectionRow>
    {!legacy && <SectionRow className="flex-wrap">
      <div className="min-w-0 flex-1">
        <div className="text-ink">个人访问令牌</div>
        <div className="mt-0.5 font-mono text-xs text-muted">
          {me.token ? `msime_pat_••••${me.token.last4} · ${expiry}` : "尚未生成 · 用于脚本和 API 调用，有效期 30 天"}
        </div>
      </div>
      <Button size="sm" variant="outline" className="h-8 rounded-[9px] px-3 text-[13px]" disabled={!canRegenerate || regenerate.isPending}
        title={canRegenerate ? undefined : "只能在浏览器登录会话中生成令牌，令牌不能为自己续期"}
        onClick={async () => {
          if (me.token && await confirm({ title: "重新生成个人访问令牌？", description: "旧令牌会立即失效，使用它的脚本需要换成新令牌。", okLabel: "重新生成", tone: "primary" }) === null) return;
          regenerate.mutate();
        }}>{me.token ? "重新生成" : "生成"}</Button>
      {issued && <div className="w-full rounded-xl bg-accent-soft px-3.5 py-3">
        <div className="text-xs font-semibold text-accent-ink">新令牌只显示这一次，请立即复制保存</div>
        <div className="mt-2 flex flex-wrap items-center gap-2">
          <code className="min-w-0 flex-1 break-all rounded-lg bg-panel px-2.5 py-1.5 font-mono text-xs text-ink ring-1 ring-hair">{issued}</code>
          <Button size="sm" variant="outline" onClick={async () => {
            try {
              await navigator.clipboard.writeText(issued);
              toast("已复制到剪贴板");
            } catch {
              toast("复制失败，请手动选择令牌复制");
            }
          }}><Copy size={14} aria-hidden="true" />复制</Button>
          <Button size="sm" variant="ghost" onClick={() => setIssued(null)}>完成</Button>
        </div>
        <div className="mt-2 text-xs text-muted">在请求头中使用：Authorization: Bearer &lt;令牌&gt;</div>
      </div>}
    </SectionRow>}
  </SectionCard>;
}

function Sessions({ me }: { me: Me }) {
  const run = useMeAction();
  const client = useQueryClient();
  const toast = useToast();
  const revoke = useMutation({
    mutationFn: (session: MeSession) => run({ action: "revoke_session", id: session.id }),
    onSuccess: (_r, session) => toast(`已退出 ${session.device}`),
    onError: error => toast(`操作失败：${errorMessage(error)}`),
    onSettled: () => client.invalidateQueries({ queryKey: meKey }),
  });
  return <SectionCard title="登录设备">
    {me.sessions.length === 0 ? <Empty title={me.via === "token" ? "当前通过个人访问令牌访问，没有有效的浏览器会话" : "没有有效的登录会话"} /> : me.sessions.map(session => <SectionRow key={session.id}>
      <div className="min-w-0 flex-1">
        <div className="truncate text-ink">{session.device}</div>
        <div className="mt-0.5 text-xs text-muted">{session.current ? "当前会话" : `最近活动 ${relativeTime(session.last_seen_at)}`} · 登录于 {format(new Date(session.created_at), "MM-dd HH:mm")}</div>
      </div>
      {session.current
        ? <span className="shrink-0 text-xs font-semibold text-accent-ink">当前设备</span>
        : <Button size="sm" variant="danger-outline" className="h-[30px] rounded-[9px]" disabled={revoke.isPending && revoke.variables?.id === session.id} onClick={() => revoke.mutate(session)}>退出</Button>}
    </SectionRow>)}
  </SectionCard>;
}

function Recent({ me }: { me: Me }) {
  return <SectionCard title="我的最近操作">
    {me.recent.length === 0 ? <Empty title="还没有操作记录" /> : me.recent.map(entry => <SectionRow key={entry.id} className="py-3">
      <span className="min-w-0 flex-1 text-ink">{describeAudit(entry)}</span>
      <span className="shrink-0 text-xs text-muted" title={new Date(entry.created_at).toLocaleString("zh-CN")}>{relativeTime(entry.created_at)}</span>
    </SectionRow>)}
  </SectionCard>;
}

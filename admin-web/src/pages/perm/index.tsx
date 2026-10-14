import { useState } from "react";
import type { FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useAuth } from "../../auth";
import { errorMessage, isAPIError, useAPI } from "../../api/client";
import { keys } from "../../api/keys";
import { actorName, auditListSchema, changeResultSchema, describeAudit, initial, permissionLabel, permissionsSchema } from "../../api/perm";
import type { Member, Permissions, Role } from "../../api/perm";
import { roleLabel } from "../../api/shell";
import { PageIntro } from "../../shell/page-intro";
import { noPermissionHint, usePermissions } from "../../shell/permissions";
import { Button } from "../../ui/button";
import { cn } from "../../ui/cn";
import { useConfirm } from "../../ui/confirm";
import { Pill } from "../../ui/pill";
import { Empty, ErrorState, SkeletonRows } from "../../ui/states";
import { useToast } from "../../ui/toast";
import { relativeTime } from "../../utils/time";
import { Avatar, SectionCard, SectionRow } from "../me/section";

const permKey = keys.page("perm", "matrix");

export default function PermPage() {
  const api = useAPI();
  const query = useQuery({ queryKey: permKey, queryFn: ({ signal }) => api.get("permissions", permissionsSchema, { signal }) });
  return <>
    <PageIntro page="perm" />
    <div className="grid gap-3.5">
      <div className="grid items-start gap-3.5 min-[1100px]:grid-cols-2">
        <SectionCard title="角色与权限">
          {query.isPending ? <SkeletonRows rows={8} /> : query.isError ? <ErrorState className="m-4" error={query.error} onRetry={() => query.refetch()} /> : <Matrix data={query.data} />}
        </SectionCard>
        <AuditLog />
      </div>
      <SectionCard title="管理员">
        {query.isPending ? <SkeletonRows rows={3} /> : query.isError ? <ErrorState className="m-4" error={query.error} onRetry={() => query.refetch()} /> : <Members data={query.data} />}
      </SectionCard>
    </div>
  </>;
}

function Matrix({ data }: { data: Permissions }) {
  const api = useAPI();
  const client = useQueryClient();
  const toast = useToast();
  const { can } = usePermissions();
  const allowed = can("manage_permissions");
  const change = useMutation({
    mutationFn: (v: { role: Role; permission: string; grant: boolean }) => api.post("permissions", changeResultSchema, { action: v.grant ? "grant" : "revoke", role: v.role.key, permission: v.permission }),
    onSuccess: (_r, v) => toast(`${v.grant ? "已授予" : "已收回"}「${v.role.name}」：${permissionLabel(v.permission)}`),
    onError: (error, v) => toast(isAPIError(error, "protected") && v.role.key === "maintainer" ? "维护者必须保留权限管理" : `操作失败：${errorMessage(error)}`),
    // The caller's own role may have changed, so the shell's permissions are refreshed too.
    onSettled: () => Promise.all([client.invalidateQueries({ queryKey: keys.page("perm") }), client.invalidateQueries({ queryKey: keys.shell })]),
  });
  return <div className="overflow-x-auto px-5 py-4">
    <table className="w-full min-w-[460px] table-fixed border-separate border-spacing-x-0 border-spacing-y-1 text-sm" aria-label="角色权限矩阵">
      <thead>
        <tr>
          <th scope="col" className="w-[32%] pb-1 text-left text-xs font-normal text-muted">权限</th>
          {data.roles.map(role => <th scope="col" key={role.key} className="pb-1 text-center text-xs font-normal text-muted">{role.name}</th>)}
        </tr>
      </thead>
      <tbody>
        {data.permissions.map(permission => <tr key={permission}>
          <th scope="row" className="py-1.5 text-left font-normal text-ink">{permissionLabel(permission)}</th>
          {data.roles.map(role => {
            const granted = data.matrix[role.key]?.includes(permission) ?? false;
            const pending = change.isPending && change.variables?.role.key === role.key && change.variables.permission === permission;
            return <td key={role.key} className="text-center">
              <button type="button" aria-pressed={granted} aria-label={`${role.name}：${permissionLabel(permission)}`}
                disabled={!allowed || pending} title={allowed ? undefined : noPermissionHint}
                className={cn("inline-flex h-[22px] w-[22px] items-center justify-center rounded-[7px] align-middle text-xs font-bold text-btn-fg transition hover:opacity-85 disabled:cursor-not-allowed disabled:hover:opacity-100",
                  granted ? "bg-accent" : "bg-transparent inset-ring-[1.5px] inset-ring-hair-2", pending && "opacity-50")}
                onClick={() => {
                  if (granted && role.key === "maintainer" && permission === "manage_permissions") {
                    toast("维护者必须保留权限管理");
                    return;
                  }
                  change.mutate({ role, permission, grant: !granted });
                }}>{granted ? "✓" : ""}</button>
            </td>;
          })}
        </tr>)}
      </tbody>
    </table>
    {!allowed && <p className="m-0 mt-3 text-xs text-muted">只有拥有「管理权限」的角色可以修改矩阵；每次修改都会写入操作日志。</p>}
  </div>;
}

function AuditLog() {
  const api = useAPI();
  const [page, setPage] = useState(1);
  const query = useQuery({ queryKey: keys.page("perm", "audit", page), queryFn: ({ signal }) => api.get(`audit?page=${page}`, auditListSchema, { signal }), placeholderData: previous => previous });
  const data = query.data;
  return <SectionCard title="操作日志" actions={data && (data.has_more || page > 1) && <>
    <Button size="sm" variant="ghost" disabled={page <= 1 || query.isFetching} onClick={() => setPage(p => p - 1)}>上一页</Button>
    <span className="text-xs text-muted tabular-nums">{page} / {Math.max(1, Math.ceil(data.total / 50))}</span>
    <Button size="sm" variant="ghost" disabled={!data.has_more || query.isFetching} onClick={() => setPage(p => p + 1)}>下一页</Button>
  </>}>
    {query.isPending ? <SkeletonRows rows={6} /> : query.isError ? <ErrorState className="m-4" error={query.error} onRetry={() => query.refetch()} />
      : data && data.items.length === 0 ? <Empty title="还没有操作记录">审核、封禁、发布和权限变更都会记录在这里。</Empty>
      : data?.items.map(entry => {
        const who = actorName(entry.actor);
        return <SectionRow key={entry.id} className="items-start py-3">
          <Avatar letter={initial(who)} />
          <div className="min-w-0 flex-1">
            <div className="break-words text-ink"><b className="font-semibold">{who}</b> {describeAudit(entry)}</div>
            <div className="mt-0.5 text-xs text-muted" title={new Date(entry.created_at).toLocaleString("zh-CN")}>{relativeTime(entry.created_at)}</div>
          </div>
        </SectionRow>;
      })}
  </SectionCard>;
}

const selectClass = "h-8 rounded-[9px] bg-panel px-2 text-[13px] text-ink inset-ring inset-ring-hair-2 disabled:opacity-45";

function Members({ data }: { data: Permissions }) {
  const api = useAPI();
  const client = useQueryClient();
  const toast = useToast();
  const confirm = useConfirm();
  const { session } = useAuth();
  const manage = session?.can_manage_admins === true;
  const [email, setEmail] = useState("");
  const [role, setRole] = useState("reviewer");
  const change = useMutation({
    mutationFn: (v: { action: "add" | "enable" | "disable" | "revoke" | "set_role"; email: string; role?: string }) => api.post("admins", changeResultSchema, v),
    onSuccess: (_r, v) => {
      const name = v.email.split("@")[0];
      const text = { add: `已添加管理员 ${name}`, enable: `已重新启用 ${name}`, disable: `已停用 ${name}`, revoke: `已撤销 ${name} 的全部会话`, set_role: `已将 ${name} 设为「${roleLabel(v.role ?? "")}」` }[v.action];
      toast(text);
      if (v.action === "add") setEmail("");
    },
    onError: error => toast(`操作失败：${errorMessage(error)}`),
    onSettled: () => client.invalidateQueries({ queryKey: keys.page("perm") }),
  });
  const submit = (event: FormEvent) => {
    event.preventDefault();
    const value = email.trim().toLowerCase();
    if (value) change.mutate({ action: "add", email: value, role });
  };
  const act = async (member: Member, action: "enable" | "disable" | "revoke") => {
    const copy = {
      enable: { title: `重新启用 ${member.email}？`, description: "该账号需要重新登录，之前的会话和访问令牌不会恢复。", okLabel: "启用", tone: "primary" as const },
      disable: { title: `停用 ${member.email}？`, description: "立即拒绝该账号的后续请求，并注销全部会话和个人访问令牌。", okLabel: "停用", tone: "danger" as const },
      revoke: { title: `撤销 ${member.email} 的全部会话？`, description: "该账号的浏览器会话和个人访问令牌立即失效，需要重新登录。", okLabel: "撤销", tone: "danger" as const },
    }[action];
    if (await confirm(copy) !== null) change.mutate({ action, email: member.email });
  };
  const roleOptions = data.roles.map(r => <option key={r.key} value={r.key}>{r.name}</option>);
  return <>
    {manage ? <form onSubmit={submit} className="flex flex-wrap items-center gap-2 border-b border-hair px-5 py-3">
      <label className="sr-only" htmlFor="perm-member-email">Google 账号邮箱</label>
      <input id="perm-member-email" type="email" required maxLength={254} value={email} onChange={event => setEmail(event.target.value)} placeholder="Google 账号邮箱"
        className="h-9 min-w-[200px] flex-1 rounded-[10px] bg-panel px-3 text-[13.5px] text-ink outline-none inset-ring inset-ring-hair-2 placeholder:text-muted focus:inset-ring-accent" />
      <label className="sr-only" htmlFor="perm-member-role">角色</label>
      <select id="perm-member-role" value={role} onChange={event => setRole(event.target.value)} className={cn(selectClass, "h-9")}>{roleOptions}</select>
      <Button type="submit" variant="primary" disabled={change.isPending}>添加管理员</Button>
      <p className="m-0 w-full text-xs text-muted">仅支持 Google 登录，最多 100 名管理员。停用后立即拒绝后续请求；角色变更在下一次请求时生效。</p>
    </form> : <p className="m-0 border-b border-hair px-5 py-3 text-xs text-muted">只有部署配置中的所有者可以添加、停用成员或调整成员角色。</p>}
    {data.members.length === 0 ? <Empty title="还没有管理员" /> : data.members.map(member => {
      const name = member.email.split("@")[0] || member.email;
      const meta = [member.owner ? "部署配置的所有者" : "", member.sessions ? `${member.sessions} 个有效会话` : "无有效会话", member.last_seen_at ? `最近活动 ${relativeTime(member.last_seen_at)}` : ""].filter(Boolean).join(" · ");
      return <SectionRow key={member.email} className="flex-wrap">
        <Avatar letter={initial(name)} />
        <div className="min-w-[180px] flex-1">
          <div className="flex flex-wrap items-center gap-2 text-ink"><span className="break-all">{member.email}</span>{!member.enabled && <Pill tone="warn">已停用</Pill>}</div>
          <div className="mt-0.5 text-xs text-muted">{meta}</div>
        </div>
        {member.owner || !manage
          ? <Pill tone={member.owner ? "info" : "ok"}>{roleLabel(member.role)}</Pill>
          : <>
            <label className="sr-only" htmlFor={`role-${member.email}`}>{`${member.email} 的角色`}</label>
            <select id={`role-${member.email}`} value={member.role} disabled={change.isPending} className={selectClass}
              onChange={event => change.mutate({ action: "set_role", email: member.email, role: event.target.value })}>{roleOptions}</select>
            <Button size="sm" variant={member.enabled ? "danger-outline" : "outline"} disabled={change.isPending} onClick={() => act(member, member.enabled ? "disable" : "enable")}>{member.enabled ? "停用" : "启用"}</Button>
            <Button size="sm" variant="ghost" disabled={change.isPending} onClick={() => act(member, "revoke")}>撤销会话</Button>
          </>}
      </SectionRow>;
    })}
  </>;
}

import { formatNumber } from "../../utils/number";
import { useQuery } from "@tanstack/react-query";
import { isGithubDisabled, useAPI } from "../../api/client";
import { keys } from "../../api/keys";
import { crashGroupDetailSchema, crashIssueTargetSchema, crashStatusLabel, crashStatusTone, crashTrend, platformName } from "../../api/crash";
import type { CrashGroup, CrashStatus } from "../../api/crash";
import { formatDateTime, relativeTime } from "../../utils/time";
import { noPermissionHint } from "../../shell/permissions";
import { DetailDrawer } from "../../ui/drawer";
import type { DrawerAction, DrawerField } from "../../ui/drawer";
import { ErrorState, SkeletonRows } from "../../ui/states";

export type CrashDrawerProps = {
  signature: string | null;
  // override is the optimistic status while a change is pending.
  override?: CrashStatus;
  // issueURL is an issue opened on GitHub that was not written back to the group.
  issueURL?: string;
  canTriage: boolean;
  onClose: () => void;
  onStatus: (group: CrashGroup, to: CrashStatus, text: string) => void;
  onCreateIssue: (group: CrashGroup) => void;
};

// CrashDrawer is the 查看堆栈 view: the group's counts, where its issue goes, and the latest 5 crashes with their stacks.
export function CrashDrawer({ signature, override, issueURL, canTriage, onClose, onStatus, onCreateIssue }: CrashDrawerProps) {
  const api = useAPI();
  const detail = useQuery({
    queryKey: keys.page("crash", "group", signature),
    queryFn: ({ signal }) => api.get(`crash-groups/${signature}`, crashGroupDetailSchema, { signal }),
    enabled: Boolean(signature),
  });
  const target = useQuery({
    queryKey: keys.page("crash", "issue-target", signature),
    queryFn: ({ signal }) => api.get(`crash-groups/${signature}/issue`, crashIssueTargetSchema, { signal }),
    enabled: Boolean(signature),
    retry: false,
    staleTime: 60_000,
  });

  const loaded = detail.data?.group;
  const group = loaded && (override || (issueURL && !loaded.issue_url)) ? { ...loaded, status: override ?? loaded.status, issue_url: loaded.issue_url ?? issueURL ?? null } : loaded;
  const githubDisabled = isGithubDisabled(target.error);
  const repo = target.data?.target?.repo;

  let repoText = "读取中…";
  if (githubDisabled) repoText = "未配置（admin.github）";
  else if (target.isError) repoText = "读取失败";
  else if (target.data) repoText = repo ?? "未配置（该平台没有仓库）";

  const fields: DrawerField[] = group ? [
    { label: "签名", value: group.signature, mono: true },
    { label: "平台 / 最新版本", value: `${platformName(group.platform)} · ${group.version}` },
    { label: "近 7 天", value: `${formatNumber(group.count_7d)} 次` },
    { label: "前 7 天", value: `${formatNumber(group.count_prev_7d)} 次（${crashTrend(group).text}）` },
    { label: "影响设备", value: group.devices_7d === null ? "客户端未上报" : `${formatNumber(group.devices_7d)} 台` },
    { label: "Issue 仓库", value: repoText, mono: Boolean(repo) },
    { label: "首次出现", value: formatDateTime(group.first_seen) },
    { label: "最近出现", value: `${formatDateTime(group.last_seen)}（${relativeTime(group.last_seen)}）` },
    ...(group.issue_url ? [{ label: "Issue", value: <a className="text-accent-ink underline underline-offset-2" href={group.issue_url} target="_blank" rel="noopener noreferrer">{group.issue_url.replace(/^https:\/\/github\.com\//, "")}</a> }] : []),
  ] : [];

  const actions: DrawerAction[] = [];
  if (group) {
    const denied = !canTriage;
    if (group.issue_url) {
      const url = group.issue_url;
      actions.push({ label: "在 GitHub 打开", keepOpen: true, onClick: () => window.open(url, "_blank", "noopener,noreferrer") });
    }
    if (group.status === "open") {
      actions.push({ label: "标记为已知", disabled: denied, onClick: () => onStatus(group, "known", "已标记为已知问题") });
      if (!group.issue_url) actions.push({ label: "建 Issue", variant: "primary", disabled: denied || githubDisabled || !repo, onClick: () => onCreateIssue(group) });
    } else if (group.status === "known") {
      actions.push({ label: "重新打开", disabled: denied, onClick: () => onStatus(group, "open", "已重新打开") });
      actions.push({ label: "标记为已修复", variant: "primary", disabled: denied, onClick: () => onStatus(group, "fixed", "已标记为已修复") });
    } else {
      actions.push({ label: "重新打开", disabled: denied, onClick: () => onStatus(group, "open", "已重新打开") });
    }
  }

  const samples = detail.data?.samples ?? [];
  return <DetailDrawer
    open={Boolean(signature)}
    onClose={onClose}
    title={group ? <span className="font-mono text-[15px] [overflow-wrap:anywhere]">{group.title || group.signature}</span> : "崩溃分组"}
    sub={group ? `${platformName(group.platform)} · 最近 ${relativeTime(group.last_seen)}` : undefined}
    pills={group ? [{ text: crashStatusLabel[group.status], tone: crashStatusTone[group.status] }, { text: platformName(group.platform), tone: "mute" }] : undefined}
    fields={fields}
    sections={group ? [{
      title: `最近 ${samples.length} 条样本`,
      content: samples.length === 0
        ? <div className="rounded-[10px] bg-panel-2 px-3 py-2.5 text-[13.5px] text-muted">原始上报已被清理，只保留了分组。</div>
        : <div className="grid gap-2">{samples.map(sample => <div key={sample.id} className="min-w-0 rounded-[10px] bg-panel-2 px-3 py-2.5">
          <div className="text-[13.5px] leading-[1.7] [overflow-wrap:anywhere] whitespace-pre-wrap text-ink">{sample.message}</div>
          <div className="mt-0.5 text-xs text-muted"><span className="font-mono">{sample.version}</span> · {formatDateTime(sample.created_at)}{sample.resolved ? " · 已解决" : ""}</div>
          {sample.stack.trim()
            ? <pre className="m-0 mt-2 max-h-64 overflow-auto rounded-lg bg-panel px-2.5 py-2 font-mono text-[11.5px] leading-[1.6] text-body ring-1 ring-hair">{sample.stack}</pre>
            : <div className="mt-2 text-xs text-muted">这条上报没有堆栈。</div>}
        </div>)}</div>,
    }] : undefined}
    actions={actions}
  >
    {detail.isPending && signature && <SkeletonRows rows={4} />}
    {detail.isError && <ErrorState error={detail.error} onRetry={() => void detail.refetch()} />}
    {group && !canTriage && <p className="m-0 mb-3 text-xs text-muted">{noPermissionHint}（需要「问题分诊」权限）</p>}
  </DetailDrawer>;
}

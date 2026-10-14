import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { Dispatch, ReactNode, SetStateAction } from "react";
import { errorMessage, useAPI } from "../../api/client";
import type { Entry, PRState, PRSummary } from "../../api/dictpr";
import { approveSchema, authorLabel, flagLabels, flagTones, kindLabels, prDetailSchema, rejectSchema, stateLabels, stateTones, trimSchema } from "../../api/dictpr";
import { keys } from "../../api/keys";
import { usePageHotkeys } from "../../shell/hotkeys";
import { relativeTime } from "../../utils/time";
import { noPermissionHint, usePermissions } from "../../shell/permissions";
import { Button, buttonVariants } from "../../ui/button";
import { Banner, Card } from "../../ui/card";
import { cn } from "../../ui/cn";
import { useConfirm } from "../../ui/confirm";
import { Pill } from "../../ui/pill";
import { Empty, ErrorState, SkeletonRows } from "../../ui/states";
import { useToast } from "../../ui/toast";

// Overrides maps "<number>@<head sha>" to the entries whose checkbox the reviewer changed; keying by head means a new commit on the pull request starts from the defaults again.
export type Overrides = Record<string, Record<number, boolean>>;

const rejectReasons = ["含敏感或导流内容", "拼音不规范", "全部为重复词条", "不属于通用词汇"] as const;
const entryColumns = "grid-cols-[28px_minmax(90px,1fr)_minmax(120px,1.4fr)_80px_minmax(120px,1fr)]";

type Props = {
  pr: PRSummary;
  repo: string;
  overrides: Overrides;
  setOverrides: Dispatch<SetStateAction<Overrides>>;
  onNext: () => void;
  onPrevious: () => void;
  markOptimistic: (number: number, state: PRState | null) => void;
};

export function PRDetailCard({ pr, repo, overrides, setOverrides, onNext, onPrevious, markOptimistic }: Props) {
  const api = useAPI();
  const client = useQueryClient();
  const toast = useToast();
  const confirm = useConfirm();
  const { can } = usePermissions();
  const allowed = can("review_dict_pr");
  const number = pr.number;

  const detail = useQuery({
    queryKey: keys.page("dictpr", "detail", number),
    queryFn: ({ signal }) => api.get(`dict-prs/${number}`, prDetailSchema, { signal }),
  });
  const data = detail.data;
  const entries = data?.entries ?? [];
  const overrideKey = data ? `${number}@${data.head_sha}` : "";
  // The list carries the optimistic state of a delayed approval or rejection; the detail only knows GitHub's.
  const state = pr.state;
  const open = state === "open" && data?.pull.state === "open";
  // A decided pull request shows what it ended with: a merged one shipped every entry it still adds, a rejected one none.
  const isChecked = (entry: Entry) => open ? overrides[overrideKey]?.[entry.index] ?? entry.flag === "new" : state === "merged";
  const keep = entries.filter(isChecked).map(entry => entry.index);
  const counts = data?.pull.counts ?? { total: entries.length, new: 0, dup: 0, flagged: 0 };

  const refresh = () => Promise.all([client.invalidateQueries({ queryKey: keys.page("dictpr") }), client.invalidateQueries({ queryKey: keys.shell })]);
  const toggle = (entry: Entry) => setOverrides(previous => ({ ...previous, [overrideKey]: { ...previous[overrideKey], [entry.index]: !isChecked(entry) } }));

  const trim = useMutation({
    mutationFn: (body: { keep: number[]; head_sha: string }) => api.post(`dict-prs/${number}/trim`, trimSchema, body),
    onSuccess: result => {
      // Every entry the trimmed branch still adds is one the reviewer kept, so they stay checked on the new head even where the check result alone would leave them unchecked (a duplicate the reviewer chose to keep).
      setOverrides(previous => ({ ...previous, [`${number}@${result.head_sha}`]: Object.fromEntries(Array.from({ length: result.count }, (_, index) => [index, true])) }));
      toast(`已在 #${number} 推送修改：只保留勾选的 ${result.count} 条`);
    },
    onError: error => toast(`操作失败：${errorMessage(error)}`),
    onSettled: refresh,
  });
  // Merging and rejecting cannot be taken back on GitHub, so both wait 4s behind the toast's 撤销 before the request is sent.
  const delayed = (label: string, next: PRState, send: (keepalive: boolean) => Promise<unknown>) => {
    markOptimistic(number, next);
    toast({
      text: label,
      delayCommit: ({ keepalive }) => send(keepalive).then(refresh).finally(() => markOptimistic(number, null)),
      undo: () => markOptimistic(number, null),
      // The toast provider already shows the failure.
      onCommitError: () => {
        markOptimistic(number, null);
        void refresh();
      },
    });
  };

  const approve = () => {
    if (!data || !open || !allowed || trim.isPending) return;
    if (keep.length === 0) {
      toast("至少勾选一条词条才能通过");
      return;
    }
    const body = { keep, head_sha: data.head_sha };
    delayed(`#${number} 已通过（${keep.length} 条）`, "merged", keepalive => api.post(`dict-prs/${number}/approve`, approveSchema, body, { keepalive }));
  };

  const reject = async () => {
    if (!data || !open || !allowed || trim.isPending) return;
    const reason = await confirm({ title: `驳回 #${number}？`, description: "提交者会收到驳回通知，PR 将被关闭。", okLabel: "驳回", reasons: rejectReasons });
    if (reason === null) return;
    // The head the reviewer saw: a submission that lands on the rolling branch meanwhile makes the server answer pr_changed instead of rejecting it unseen.
    const body = { reason, head_sha: data.head_sha };
    delayed(`#${number} 已驳回：${reason}`, "closed", keepalive => api.post(`dict-prs/${number}/reject`, rejectSchema, body, { keepalive }));
  };

  const canTrim = open && allowed && keep.length > 0 && keep.length < entries.length && !trim.isPending;

  usePageHotkeys("dictpr", { j: onNext, k: onPrevious, a: approve, r: () => void reject() });

  const meta = [authorLabel(pr), relativeTime(pr.created_at), pr.note ? `补充说明：${pr.note}` : ""].filter(Boolean).join(" · ");
  const writeTitle = allowed ? undefined : noPermissionHint;

  return <Card className="p-0">
    <div className="flex flex-wrap items-start justify-between gap-3 border-b border-hair px-[22px] py-5">
      <div className="min-w-0">
        <div className="font-mono text-xs text-muted">{repo} · #{number}</div>
        <h2 className="m-0 mt-1 text-lg font-bold text-ink [overflow-wrap:anywhere]">{pr.title}</h2>
        <p className="m-0 mt-1.5 text-[13px] text-muted [overflow-wrap:anywhere]">{meta}</p>
      </div>
      <div className="flex shrink-0 items-center gap-2">
        <a href={pr.url} target="_blank" rel="noopener noreferrer" className={buttonVariants({ variant: "ghost", size: "sm" })}>在 GitHub 打开</a>
        <Pill tone={stateTones[state]}>{stateLabels[state]}</Pill>
      </div>
    </div>

    {detail.isPending ? <SkeletonRows rows={6} /> : detail.isError ? <ErrorState className="m-5" error={detail.error} onRetry={() => detail.refetch()} /> : <>
      <div className="grid grid-cols-2 gap-2.5 px-[22px] pt-4 min-[820px]:grid-cols-4">
        <Tile label="词条" value={counts.total} className="bg-panel-2 text-ink" />
        <Tile label="可收录" value={counts.new} className="bg-accent-soft text-accent-ink" />
        <Tile label="重复（去重）" value={counts.dup} className="bg-panel-2 text-muted" />
        <Tile label="敏感 / 不规范" value={counts.flagged} className="bg-bad-soft text-bad" />
      </div>
      {open && data?.mergeable === false && <Banner tone="warn" className="mx-[22px] mt-3">GitHub 报告这个 PR 与主干有冲突，需要先在 GitHub 上解决冲突才能合并。</Banner>}

      <div className="mt-2 overflow-x-auto px-[22px]">
        {entries.length === 0 ? <Empty title="这个 PR 没有新增词条" /> :
          <div className="min-w-[520px]">
            <div aria-hidden="true" className={cn("grid items-center gap-3 border-b border-hair py-2.5 text-xs font-semibold text-muted", entryColumns)}>
              <span />
              <span>词条</span>
              <span>拼音</span>
              <span>类型</span>
              <span>检查结果</span>
            </div>
            <ul className="m-0 list-none p-0" aria-label={`#${number} 的词条`}>
              {entries.map(entry => {
                const checked = isChecked(entry);
                return <li key={entry.index} className="border-b border-hair last:border-b-0">
                  <label className={cn("grid cursor-pointer items-center gap-3 py-2.5 transition", entryColumns, open && !checked && "opacity-55", !open && "cursor-default")}>
                    <input type="checkbox" className="h-4 w-4 accent-accent" checked={checked} disabled={!open} onChange={() => toggle(entry)} aria-label={`收录「${entry.word}」`} />
                    <span className="text-[15px] font-medium text-ink [overflow-wrap:anywhere]">{entry.word}</span>
                    <span className={cn("text-[13px] text-body [overflow-wrap:anywhere]", entry.kind === "words" && "font-mono")}>{entry.pinyin || "—"}</span>
                    <span className="text-[12.5px] text-muted">{kindLabels[entry.kind]}</span>
                    <span><Pill tone={flagTones[entry.flag]} title={entry.reason}>{flagLabels[entry.flag]}</Pill></span>
                  </label>
                </li>;
              })}
            </ul>
          </div>}
      </div>

      {data && data.submissions.length > 0 && <div className="px-[22px] pt-3">
        <div className="text-[13px] font-bold text-ink">投稿记录</div>
        <ul className="m-0 mt-2 grid list-none gap-1.5 p-0">
          {data.submissions.map(submission => <li key={`${submission.created_at}-${submission.kind}`} className="rounded-[10px] bg-panel-2 px-3 py-2 text-[13px] leading-[1.7] text-body">
            <span className="text-muted">{kindLabels[submission.kind as Entry["kind"]] ?? submission.kind} · {relativeTime(submission.created_at)}</span>
            {submission.note && <span className="[overflow-wrap:anywhere]"> · {submission.note}</span>}
          </li>)}
        </ul>
      </div>}
    </>}

    <div className="mt-4 flex flex-wrap items-center gap-3 border-t border-hair px-[22px] py-4">
      <p className="m-0 min-w-0 flex-1 basis-[220px] text-[13px] text-muted">已勾选 {keep.length} / {entries.length} 条 · 未勾选的词条会在合并前从 PR 中移除</p>
      <div className="flex flex-wrap gap-2">
        <Button size="lg" variant="danger-outline" disabled={!data || !open || !allowed || trim.isPending} title={writeTitle} onClick={() => void reject()}>驳回</Button>
        <Button size="lg" variant="outline" disabled={!canTrim} title={writeTitle} onClick={() => data && trim.mutate({ keep, head_sha: data.head_sha })}>{trim.isPending ? "正在推送…" : "仅保留勾选项"}</Button>
        <Button size="lg" variant="primary" disabled={!data || !open || !allowed || keep.length === 0 || trim.isPending} title={writeTitle} onClick={approve}>审核通过并合并</Button>
      </div>
    </div>
    <div className="flex flex-wrap items-center gap-x-4 gap-y-1.5 px-[22px] pb-4 text-xs text-muted max-[759px]:hidden">
      <Hint keycap="J">下一个</Hint>
      <Hint keycap="K">上一个</Hint>
      <Hint keycap="A">通过</Hint>
      <Hint keycap="R">驳回</Hint>
      {/* The header shows the search box from 900px only. */}
      <span className="max-[899px]:hidden"><Hint keycap="/">搜索</Hint></span>
    </div>
  </Card>;
}

function Tile({ label, value, className }: { label: string; value: number; className: string }) {
  return <div className={cn("rounded-xl p-3", className)}>
    <div className="text-xs opacity-80">{label}</div>
    <div className="mt-1 text-lg font-bold tabular-nums">{value}</div>
  </div>;
}

function Hint({ keycap, children }: { keycap: string; children: ReactNode }) {
  return <span className="inline-flex items-center gap-1.5">
    <kbd className="inline-grid h-[18px] min-w-[18px] place-items-center rounded-[5px] px-1 font-mono text-[11px] text-body ring-1 ring-hair-2">{keycap}</kbd>
    {children}
  </span>;
}

import { formatNumber } from "../../utils/number";
import { useEffect, useMemo, useRef, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { errorMessage, useAPI } from "../../api/client";
import { keys } from "../../api/keys";
import { noticeChannels, noticePlatforms, noticeBodyMax, noticesSchema } from "../../api/notice";
import type { Notice, NoticeStatus, NoticeValue } from "../../api/notice";
import { PageIntro } from "../../shell/page-intro";
import { usePageSearch } from "../../shell/page-search";
import { noPermissionHint, usePermissions } from "../../shell/permissions";
import { formatDateTime, relativeTime } from "../../utils/time";
import { Button } from "../../ui/button";
import { Card, CardHeader } from "../../ui/card";
import { cn } from "../../ui/cn";
import { useConfirm } from "../../ui/confirm";
import { FilterChips } from "../../ui/filter-chips";
import { Pill } from "../../ui/pill";
import type { Tone } from "../../ui/pill";
import { Empty, ErrorState, SkeletonRows } from "../../ui/states";
import { useToast } from "../../ui/toast";
import { NoticeFields, emptyForm, formFromNotice, formValue } from "./compose";
import type { Form } from "./compose";

const statusPill: Record<NoticeStatus, { label: string; tone: Tone }> = {
  live: { label: "已发布", tone: "ok" },
  draft: { label: "草稿", tone: "mute" },
  archived: { label: "已归档", tone: "mute" },
};

type ListFilter = "current" | "archived";

function targetsLabel(targets: string[]): string {
  if (targets.includes("all")) return "全部平台";
  return noticePlatforms.filter(p => targets.includes(p.key)).map(p => p.label).join("、") || "未选平台";
}

function channelsLabel(channels: string[]): string {
  return noticeChannels.filter(c => channels.includes(c.key)).map(c => c.label).join(" + ") || "未选渠道";
}

// noticeMeta follows the design's "{targets} · {channels} · {MM-DD} 由 {user} 发布 · 触达 {n}"; reach needs client receipts, so it is shown as —.
function noticeMeta(n: Notice): string {
  const head = `${targetsLabel(n.targets)} · ${channelsLabel(n.channels)}`;
  const published = n.published_at ? `${formatDateTime(n.published_at, "MM-dd")} 由 ${n.author} 发布` : "";
  if (n.status === "draft") return `${head} · 草稿 · ${relativeTime(n.updated_at)}更新`;
  if (n.status === "archived") return `${head} · ${published ? `${published} · ` : ""}${relativeTime(n.updated_at)}归档`;
  return `${head} · ${published} · 触达 —`;
}

function bodyTooLong(value: NoticeValue): boolean {
  return [...value.body].length > noticeBodyMax;
}

export default function NoticePage() {
  const api = useAPI();
  const client = useQueryClient();
  const toast = useToast();
  const confirm = useConfirm();
  const { can } = usePermissions();
  const { focus } = usePageSearch();
  const canPublish = can("publish_notices");

  const [form, setForm] = useState<Form>(emptyForm);
  const [filter, setFilter] = useState<ListFilter>("current");
  const [expanded, setExpanded] = useState<string | null>(null);

  const query = useQuery({
    queryKey: keys.page("notice", "list"),
    queryFn: ({ signal }) => api.get("notices", noticesSchema, { signal }),
  });
  const items = query.data?.items;
  const telegram = query.data?.telegram;

  const refresh = () => client.invalidateQueries({ queryKey: keys.page("notice") });
  const save = useMutation({
    mutationFn: (v: { id?: string; value: NoticeValue }) => api.action({ action: "save_notice_draft", ...(v.id ? { id: v.id } : {}), value: v.value }),
    onSettled: refresh,
  });
  const publish = useMutation({
    mutationFn: (v: { id?: string; value: NoticeValue }) => api.action({ action: "publish_notice", ...(v.id ? { id: v.id } : {}), value: v.value }),
    onSettled: refresh,
  });
  const archive = useMutation({
    mutationFn: (id: string) => api.action({ action: "archive_notice", id }),
    onSettled: refresh,
  });
  const busy = save.isPending || publish.isPending || archive.isPending;

  // 搜索和通知通过 ?focus=<id> 打开草稿或展开公告；等待当前写入完成后再切换编辑器。
  const handledFocus = useRef<string | undefined>(undefined);
  useEffect(() => {
    if (busy || !focus || !items || handledFocus.current === focus) return;
    handledFocus.current = focus;
    const notice = items.find(n => n.id === focus);
    if (!notice) return;
    if (notice.status === "draft") setForm(formFromNotice(notice));
    setFilter(notice.status === "archived" ? "archived" : "current");
    setExpanded(notice.id);
    window.requestAnimationFrame(() => document.getElementById(`notice-${notice.id}`)?.scrollIntoView({ block: "center" }));
  }, [busy, focus, items]);

  const counts = useMemo(() => ({
    current: items?.filter(n => n.status !== "archived").length ?? 0,
    archived: items?.filter(n => n.status === "archived").length ?? 0,
  }), [items]);
  const visible = useMemo(() => items?.filter(n => (filter === "archived") === (n.status === "archived")), [items, filter]);

  // checked validates the form the way the server does and returns the value to send, or null after telling the user what is missing.
  const checked = (publishing: boolean): NoticeValue | null => {
    const value = formValue(form);
    let problem = "";
    if (!value.title) problem = "请先填写标题";
    else if (value.targets.length === 0) problem = "请选择投放范围";
    else if (publishing && value.channels.length === 0) problem = "请至少选择一个渠道";
    else if (bodyTooLong(value)) problem = `正文最多 ${formatNumber(noticeBodyMax)} 字，请缩短正文。`;
    if (problem) {
      toast(problem);
      return null;
    }
    return value;
  };

  const onDraft = async () => {
    const value = checked(false);
    if (!value) return;
    try {
      await save.mutateAsync({ id: form.id, value });
    } catch (error) {
      toast(`操作失败：${errorMessage(error)}`);
      return;
    }
    // Saving keeps the chosen targets and channels for the next notice, as in the design.
    setForm({ ...form, id: undefined, title: "", body: "" });
    toast("已存为草稿");
  };

  const onPublish = async () => {
    const value = checked(true);
    if (!value) return;
    if (value.channels.includes("telegram")) {
      const ok = await confirm({ title: `发布「${value.title}」？`, description: "公告会立即推送到 Telegram 频道，推送后无法撤回。", okLabel: "发布", tone: "primary" });
      if (ok === null) return;
    }
    try {
      await publish.mutateAsync({ id: form.id, value });
    } catch (error) {
      toast(`操作失败：${errorMessage(error)}`);
      return;
    }
    setForm(emptyForm);
    toast("公告已发布");
  };

  const onArchive = async (notice: Notice) => {
    const draft = notice.status === "draft";
    const ok = await confirm({
      title: draft ? `丢弃草稿「${notice.title}」？` : `归档「${notice.title}」？`,
      description: draft ? "草稿会移到已归档，不能再编辑或发布。" : "归档后 App 和官网会在 60 秒内停止展示这条公告；已经推送到 Telegram 的消息不会撤回。",
      okLabel: draft ? "丢弃" : "归档",
    });
    if (ok === null) return;
    try {
      await archive.mutateAsync(notice.id);
    } catch (error) {
      toast(`操作失败：${errorMessage(error)}`);
      return;
    }
    if (form.id === notice.id) setForm(emptyForm);
    toast(draft ? "草稿已丢弃" : "公告已归档");
  };

  // edit loads a draft into the editor; unsaved changes to whatever is open there are only dropped after a confirm.
  const edit = async (notice: Notice) => {
    if (form.id === notice.id) return;
    const loaded = form.id ? items?.find(n => n.id === form.id) : undefined;
    const saved = loaded ? formFromNotice(loaded) : emptyForm;
    if (JSON.stringify(formValue(form)) !== JSON.stringify(formValue(saved))) {
      const ok = await confirm({ title: "放弃未保存的修改？", description: "编辑器里有还没保存的内容，打开这条草稿会丢弃它们。", okLabel: "放弃修改" });
      if (ok === null) return;
    }
    setForm(formFromNotice(notice));
    document.getElementById("notice-title")?.focus();
  };

  const titleEmpty = !form.title.trim();
  let list = <ul className="m-0 list-none p-0">
    {visible?.map(n => <NoticeRow key={n.id} notice={n} open={expanded === n.id} editing={form.id === n.id}
      onToggle={() => setExpanded(expanded === n.id ? null : n.id)} onEdit={() => void edit(n)}
      onArchive={() => onArchive(n)} canArchive={canPublish} busy={busy} />)}
  </ul>;
  if (query.isPending) list = <SkeletonRows rows={4} />;
  else if (query.isError) list = <ErrorState className="m-4" error={query.error} onRetry={() => query.refetch()} />;
  else if (!visible?.length) list = filter === "archived"
    ? <Empty title="没有已归档的公告">归档或丢弃的公告会显示在这里。</Empty>
    : <Empty title="还没有公告">发布或存为草稿后会显示在这里。</Empty>;

  return <>
    <PageIntro page="notice" />
    <div className="grid items-start gap-3.5 min-[1100px]:grid-cols-2">
      <Card>
        <fieldset disabled={busy} aria-label="编辑公告" className="m-0 min-w-0 border-0 p-0">
          <CardHeader title={form.id ? "编辑草稿" : "新建公告"}
            actions={form.id ? <Button size="sm" variant="ghost" onClick={() => setForm(emptyForm)}>取消编辑</Button> : undefined} />
          <NoticeFields form={form} onChange={setForm} telegram={telegram} />
          <div className="mt-5 flex flex-wrap justify-end gap-2">
            <Button size="lg" variant="outline" disabled={busy} onClick={onDraft}>{save.isPending ? "保存中…" : "存为草稿"}</Button>
            <Button size="lg" variant="primary" className={cn(titleEmpty && "opacity-45")} aria-disabled={titleEmpty}
              disabled={busy || !canPublish} title={canPublish ? undefined : noPermissionHint} onClick={onPublish}>
              {publish.isPending ? "发布中…" : "发布"}
            </Button>
          </div>
        </fieldset>
      </Card>

      <Card className="p-0">
        <CardHeader title="已发布与草稿" className="mb-0 border-b border-hair px-5 py-4"
          actions={<FilterChips label="公告状态" value={filter} onChange={setFilter}
            options={[{ key: "current", label: "当前", count: items ? counts.current : undefined }, { key: "archived", label: "已归档", count: items ? counts.archived : undefined }]} />} />
        {list}
      </Card>
    </div>
  </>;
}

function NoticeRow({ notice, open, editing, onToggle, onEdit, onArchive, canArchive, busy }: {
  notice: Notice; open: boolean; editing: boolean; onToggle: () => void; onEdit: () => void; onArchive: () => void; canArchive: boolean; busy: boolean;
}) {
  const pill = statusPill[notice.status];
  return <li id={`notice-${notice.id}`} className={cn("border-b border-hair px-5 py-3.5 last:border-b-0", editing && "bg-accent-soft")}>
    <div className="flex items-start gap-3">
      <button type="button" aria-expanded={open} onClick={onToggle} className="min-w-0 flex-1 cursor-pointer text-left">
        <span className="block font-semibold text-ink [overflow-wrap:anywhere]">{notice.title}</span>
        <span className="mt-1 block text-[12.5px] text-muted" title={notice.status === "live" ? "触达人数需要客户端回执，暂不统计" : undefined}>{noticeMeta(notice)}</span>
      </button>
      <Pill tone={pill.tone} className="h-5 px-2 text-[11.5px]">{pill.label}</Pill>
    </div>
    {open && <div className="mt-2.5 grid gap-2.5">
      <p className="m-0 whitespace-pre-wrap rounded-[10px] bg-panel-2 px-3 py-2.5 text-[13.5px] leading-[1.7] text-body [overflow-wrap:anywhere]">{notice.body || "（无正文）"}</p>
      {notice.status !== "archived" && <div className="flex flex-wrap justify-end gap-2">
        {notice.status === "draft" && <Button size="sm" disabled={busy} onClick={onEdit}>编辑</Button>}
        <Button size="sm" variant="danger-outline" disabled={busy || !canArchive} title={canArchive ? undefined : noPermissionHint} onClick={onArchive}>
          {notice.status === "draft" ? "丢弃" : "归档"}
        </Button>
      </div>}
    </div>}
  </li>;
}

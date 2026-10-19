import { useState } from "react";
import type { ReactNode } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { APIError, errorMessage, useAPI } from "../../api/client";
import { keys } from "../../api/keys";
import type { Release, ReleasePlatform } from "../../api/release";
import { releaseHistorySchema, releaseOkSchema, releasePath, releasesSchema, withdrawResultSchema } from "../../api/release";
import { PageIntro } from "../../shell/page-intro";
import { usePageSearch, useSetPageSearch } from "../../shell/page-search";
import { usePermissions } from "../../shell/permissions";
import { useConfirm } from "../../ui/confirm";
import { ErrorState, NotConfigured, Skeleton } from "../../ui/states";
import { useToast } from "../../ui/toast";
import { DetailBody, DetailHeader } from "./detail";
import type { DetailHandlers } from "./detail";
import { NotesDialog, TriggerDialog } from "./dialogs";
import { PlatformCard } from "./list";

type DialogState =
  | { kind: "trigger"; platform: ReleasePlatform; version: string }
  | { kind: "notes"; platform: ReleasePlatform; release: Release }
  | null;

export default function ReleasePage() {
  const api = useAPI();
  const client = useQueryClient();
  const toast = useToast();
  const confirm = useConfirm();
  const { can } = usePermissions();
  const canTrigger = can("trigger_release");
  const search = usePageSearch();
  const setSearch = useSetPageSearch();
  // ?focus=<platform>:<tag> comes from the global search; release notifications carry the platform id alone.
  const [focusPlatform = "", focusTag = ""] = (search.focus ?? "").split(/:(.*)/s);
  const platformID = search.platform ?? (focusPlatform || undefined);
  const [dialog, setDialog] = useState<DialogState>(null);

  const list = useQuery({
    queryKey: keys.page("release", "list"),
    queryFn: ({ signal }) => api.get("releases", releasesSchema, { signal }),
  });
  const history = useQuery({
    queryKey: keys.page("release", "history", platformID),
    queryFn: ({ signal }) => api.get(releasePath(platformID ?? ""), releaseHistorySchema, { signal }),
    enabled: Boolean(platformID),
  });
  const refresh = () => client.invalidateQueries({ queryKey: keys.page("release") });
  const fail = (error: unknown) => toast(`操作失败：${errorMessage(error)}`);

  const trigger = useMutation({
    mutationFn: (v: { platform: ReleasePlatform; version: string }) => api.post(releasePath(v.platform.id, undefined, "trigger"), releaseOkSchema, { version: v.version }),
    onSuccess: (_, v) => { setDialog(null); toast(`${v.platform.name} ${v.version} 发布流水线已触发`); },
    onError: error => {
      if (error instanceof APIError && error.code === "workflow_uncertain") {
        setDialog(null);
        toast(errorMessage(error));
      } else fail(error);
    },
    onSettled: refresh,
  });
  const notes = useMutation({
    mutationFn: (v: { platform: ReleasePlatform; release: Release; body: string }) => api.post(releasePath(v.platform.id, v.release.tag, "notes"), releaseOkSchema, { body: v.body }),
    onSuccess: (_, v) => { setDialog(null); toast(`${v.platform.name} ${v.release.version} 的发布说明已更新`); },
    onError: fail,
    onSettled: refresh,
  });
  const withdraw = useMutation({
    mutationFn: (v: { platform: ReleasePlatform; release: Release }) => api.post(releasePath(v.platform.id, v.release.tag, "withdraw"), withdrawResultSchema),
    onSuccess: (result, v) => {
      const name = `${v.platform.name} ${v.release.version}`;
      if (!result.previous) toast(`${name} 已撤回，没有可回退的正式版本`);
      else if (result.was_latest && !result.latest_restored) toast(`${name} 已撤回，但未能把 ${result.previous} 设为最新版本，请在 GitHub 上手动设置`);
      else toast(`${name} 已撤回，下载页回退到上一个版本`);
    },
    onError: fail,
    onSettled: refresh,
  });

  const handlers: DetailHandlers = {
    canTrigger,
    onTrigger: (platform, version) => setDialog({ kind: "trigger", platform, version }),
    onEditNotes: (platform, release) => setDialog({ kind: "notes", platform, release }),
    onWithdraw: async (platform, release) => {
      const ok = await confirm({ title: `撤回 ${platform.name} ${release.version}？`, description: "下载页会回退到上一个正式版本，已安装的用户不受影响。", okLabel: "撤回" });
      if (ok !== null) withdraw.mutate({ platform, release });
    },
  };
  const openPlatform = (id: string | undefined) => {
    setSearch({ platform: id, focus: undefined }, { replace: false });
    window.scrollTo(0, 0);
  };

  const platforms = list.data?.platforms ?? [];
  const selected = history.data?.platform ?? platforms.find(p => p.id === platformID);
  const selectedFocusTag = focusPlatform === selected?.id && focusTag ? focusTag : undefined;

  let content: ReactNode;
  if (platformID) {
    content = <div className="flex flex-col gap-3.5">
      {selected ? <DetailHeader platform={selected} platforms={platforms.length ? platforms : [selected]} onBack={() => openPlatform(undefined)} onPick={openPlatform} />
        : <Skeleton className="h-[34px] w-64" />}
      {selected ? <DetailBody key={`${selected.id}:${selectedFocusTag ?? ""}`} platform={selected} history={history.data} loading={history.isPending} error={history.error} onRetry={() => history.refetch()}
        focusTag={selectedFocusTag} handlers={handlers} />
        : history.isError ? <ErrorState error={history.error} onRetry={() => history.refetch()} /> : <Skeleton className="h-64" />}
    </div>;
  } else if (list.isPending) {
    content = <div className="grid grid-cols-[repeat(auto-fill,minmax(300px,1fr))] gap-3.5 max-[759px]:grid-cols-1" role="status" aria-label="正在加载">
      {[0, 1, 2].map(i => <Skeleton key={i} className="h-[300px] rounded-[18px]" />)}
    </div>;
  } else if (list.isError) {
    content = <ErrorState error={list.error} onRetry={() => list.refetch()} />;
  } else if (platforms.length === 0) {
    content = <NotConfigured title="未配置发布平台">在 config.json 的 admin.github.platforms 中配置平台名称、仓库、tag 前缀和发布流水线后，这里会显示各平台的 GitHub Release。</NotConfigured>;
  } else {
    content = <div className="grid grid-cols-[repeat(auto-fill,minmax(300px,1fr))] gap-3.5 max-[759px]:grid-cols-1">
      {platforms.map(item => <PlatformCard key={item.id} item={item} canTrigger={canTrigger} onOpen={() => openPlatform(item.id)}
        onTrigger={() => handlers.onTrigger(item, item.latest && item.latest.status !== "released" ? item.latest.version : "")} />)}
    </div>;
  }

  return <>
    {!platformID && <PageIntro page="release" />}
    {content}
    {dialog?.kind === "trigger" && dialog.platform.workflow && <TriggerDialog platform={dialog.platform.name} workflow={dialog.platform.workflow} initialVersion={dialog.version}
      pending={trigger.isPending} onClose={() => setDialog(null)} onSubmit={version => trigger.mutate({ platform: dialog.platform, version })} />}
    {dialog?.kind === "notes" && <NotesDialog title={`编辑 ${dialog.platform.name} ${dialog.release.version} 的发布说明`} initialBody={dialog.release.body}
      pending={notes.isPending} onClose={() => setDialog(null)} onSubmit={body => notes.mutate({ platform: dialog.platform, release: dialog.release, body })} />}
  </>;
}

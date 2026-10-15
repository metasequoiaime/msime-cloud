import { useState } from "react";
import type { FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { errorMessage, useAPI } from "../../api/client";
import { lanzouURLError, siteSettingsSchema } from "../../api/downloads";
import { keys } from "../../api/keys";
import { noPermissionHint, usePermissions } from "../../shell/permissions";
import { Button } from "../../ui/button";
import { Card, CardHeader } from "../../ui/card";
import { useConfirm } from "../../ui/confirm";
import { useToast } from "../../ui/toast";
import { Skeleton } from "../../ui/states";

const settingsKey = keys.page("downloads", "site-settings");

// MirrorSettings edits the Lanzou cloud link that the official website's download page reads from GET /v1/site/download-mirrors. Every role sees it; saving needs publish_notices.
export function MirrorSettings() {
  const api = useAPI();
  const client = useQueryClient();
  const confirm = useConfirm();
  const toast = useToast();
  const { can } = usePermissions();
  const canEdit = can("publish_notices");
  const [draft, setDraft] = useState<string | null>(null);
  const [invalid, setInvalid] = useState<string | null>(null);
  const query = useQuery({ queryKey: settingsKey, queryFn: ({ signal }) => api.get("site-settings", siteSettingsSchema, { signal }) });
  const mutation = useMutation({
    mutationFn: (value: string) => api.post("site-settings", siteSettingsSchema, { lanzou_url: value }),
    onSuccess: data => {
      client.setQueryData(settingsKey, data);
      setDraft(null);
      toast(data.lanzou_url ? "链接已保存，官网约 10 分钟内生效" : "链接已清空，官网约 10 分钟内隐藏入口");
    },
    onError: error => toast(`保存失败：${errorMessage(error)}`),
  });
  const current = query.data?.lanzou_url ?? "";
  const value = draft ?? current;

  const save = async (next: string) => {
    const trimmed = next.trim();
    const error = lanzouURLError(trimmed);
    setInvalid(error);
    if (error) return;
    if (trimmed === "") {
      const ok = await confirm({ title: "清空蓝奏云盘链接？", description: "官网下载页将不再显示该入口。", okLabel: "清空" });
      if (ok === null) return;
    }
    mutation.mutate(trimmed);
  };
  const submit = (event: FormEvent) => { event.preventDefault(); void save(value); };
  const disabledTitle = canEdit ? undefined : noPermissionHint;

  return <Card>
    <CardHeader title="官网下载镜像" sub={<>官网下载页通过公开接口 <code>GET /v1/site/download-mirrors</code> 读取此链接，作为 Windows 安装包的国内下载入口；留空保存即隐藏该入口。</>} />
    {query.isError
      ? <p className="m-0 text-[13px] text-bad" role="alert">{errorMessage(query.error)} <Button size="sm" variant="ghost" onClick={() => void query.refetch()}>重试</Button></p>
      : <>
        <dl className="m-0 mb-3 grid grid-cols-[auto_minmax(0,1fr)] gap-x-4 gap-y-1 text-[13px]">
          <dt className="text-muted">当前链接</dt>
          <dd className="m-0 min-w-0 truncate">{query.isPending ? <Skeleton className="h-4 w-48" /> : current ? <a href={current} target="_blank" rel="noopener noreferrer" className="text-accent">{current}</a> : <span className="text-muted">未设置</span>}</dd>
          <dt className="text-muted">最近修改</dt>
          <dd className="m-0 min-w-0 truncate text-body">{query.isPending ? <Skeleton className="h-4 w-32" /> : query.data?.updated_at ? `${new Date(query.data.updated_at).toLocaleString("zh-CN")} · ${query.data.updated_by}` : "从未修改"}</dd>
        </dl>
        <form onSubmit={submit} className="flex flex-wrap gap-2">
          <input value={value} onChange={event => { setDraft(event.target.value); setInvalid(null); }} disabled={!canEdit || !query.data || mutation.isPending} title={disabledTitle}
            type="text" inputMode="url" autoComplete="off" spellCheck={false} maxLength={512} placeholder="https://www.lanzouq.com/xxxxxx" aria-label="蓝奏云盘分享链接"
            aria-invalid={invalid !== null} aria-describedby={invalid ? "lanzou-url-error" : undefined}
            className="h-9 min-w-0 flex-[1_1_260px] rounded-[10px] bg-panel-2 px-3 text-[13.5px] text-ink outline-none placeholder:text-muted focus:ring-[1.5px] focus:ring-accent disabled:opacity-45" />
          <Button type="submit" variant="primary" disabled={!canEdit || !query.data || mutation.isPending || value.trim() === current} title={disabledTitle}>{mutation.isPending ? "正在保存…" : "保存"}</Button>
          <Button variant="danger-outline" disabled={!canEdit || !query.data || mutation.isPending || !current} title={disabledTitle} onClick={() => void save("")}>清空</Button>
        </form>
        {invalid && <p id="lanzou-url-error" className="m-0 mt-2 text-[12.5px] text-bad" role="alert">{invalid}</p>}
      </>}
  </Card>;
}

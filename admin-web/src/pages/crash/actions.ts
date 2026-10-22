import { useCallback, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { APIError, useAPI } from "../../api/client";
import { createActionUndo } from "../../api/action-undo";
import type { API } from "../../api/client";
import { keys } from "../../api/keys";
import { crashIssueCreatedSchema, crashStatusUndoSchema } from "../../api/crash";
import type { CrashGroup, CrashStatus } from "../../api/crash";
import { useToast } from "../../ui/toast";

// createCrashIssue opens the group's GitHub issue; the client turns the endpoint's error codes into the failure toast's copy.
function createCrashIssue(api: API, signature: string, keepalive: boolean) {
  return api.post(`crash-groups/${signature}/issue`, crashIssueCreatedSchema, {}, { keepalive });
}

export type CrashActions = {
  // overrides are optimistic statuses shown until the server confirms (or the toast's undo cancels) a change.
  overrides: Readonly<Record<string, CrashStatus>>;
  // issueURLs are issues opened on GitHub that the server could not write back to their group (issue_not_recorded); the page shows them so 建 Issue is not offered again for a group that already has one.
  issueURLs: Readonly<Record<string, string>>;
  setStatus: (group: CrashGroup, to: CrashStatus, text: string) => Promise<void>;
  createIssue: (group: CrashGroup) => void;
};

// useCrashActions 立即提交状态并用服务端版本撤销；创建 Issue 延迟到 4 秒撤销窗口结束后提交。
export function useCrashActions(): CrashActions {
  const api = useAPI();
  const client = useQueryClient();
  const toast = useToast();
  const [overrides, setOverrides] = useState<Record<string, CrashStatus>>({});
  const [issueURLs, setIssueURLs] = useState<Record<string, string>>({});
  const clear = useCallback((signature: string) => setOverrides(previous => {
    const next = { ...previous };
    delete next[signature];
    return next;
  }), []);
  const refresh = useCallback(() => client.invalidateQueries({ queryKey: keys.page("crash") }), [client]);

  const setStatus = useCallback(async (group: CrashGroup, to: CrashStatus, text: string) => {
    let undo: (() => Promise<void>) | undefined;
    setOverrides(previous => ({ ...previous, [group.signature]: to }));
    try {
      const result = await api.action({ action: "crash_group_status", id: group.signature, value: to });
      // 旧副本没有有效版本时不提供撤销，不能从页面状态或 previous 猜反向操作。
      undo = createActionUndo(crashStatusUndoSchema, result.undo, value => value.id === group.signature, async value => {
        try {
          await api.action(value);
        } finally {
          await refresh();
        }
      });
      await refresh();
    } finally {
      clear(group.signature);
    }
    toast({ text, undo });
  }, [api, clear, refresh, toast]);

  const createIssue = useCallback((group: CrashGroup) => {
    setOverrides(previous => ({ ...previous, [group.signature]: "known" }));
    toast({
      text: "已创建 Issue 并标记为已知问题",
      delayCommit: ({ keepalive }) => createCrashIssue(api, group.signature, keepalive).then(refresh).finally(() => clear(group.signature)),
      undo: () => clear(group.signature),
      onCommitError: error => {
        clear(group.signature);
        void refresh();
        if (error instanceof APIError && error.code === "github_uncertain") {
          toast("无法确认 Issue 是否已创建，分组已保留为已知问题。请先在 GitHub 核对，确认未创建后再重新打开分组重试。");
          return;
        }
        // The issue exists on GitHub but the group does not point at it: keep its link, or the group would offer 建 Issue again and open a duplicate.
        const url = error instanceof APIError && error.code === "issue_not_recorded" ? error.details.issue_url : undefined;
        if (typeof url === "string" && url.startsWith("https://")) {
          setIssueURLs(previous => ({ ...previous, [group.signature]: url }));
          toast(`Issue 已创建但未写回分组，请勿重复创建：${url}`);
        }
      },
    });
  }, [api, clear, refresh, toast]);

  return { overrides, issueURLs, setStatus, createIssue };
}

import { useCallback, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { APIError, useAPI } from "../../api/client";
import type { API } from "../../api/client";
import { keys } from "../../api/keys";
import { crashIssueCreatedSchema, crashStatusSchema } from "../../api/crash";
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

// useCrashActions holds the page's two writes: a status change runs at once and its undo sends the reverse change; opening a GitHub issue cannot be reversed, so it is sent after the 4s undo window.
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
    let from: CrashStatus = group.status;
    setOverrides(previous => ({ ...previous, [group.signature]: to }));
    try {
      const result = await api.action({ action: "crash_group_status", id: group.signature, value: to });
      // Undo restores what the server replaced, which differs from the shown status when another admin changed it meanwhile.
      const previous = crashStatusSchema.safeParse(result.previous);
      if (previous.success) from = previous.data;
      await refresh();
    } finally {
      clear(group.signature);
    }
    toast({ text, undo: () => api.action({ action: "crash_group_status", id: group.signature, value: from }).then(refresh) });
  }, [api, clear, refresh, toast]);

  const createIssue = useCallback((group: CrashGroup) => {
    setOverrides(previous => ({ ...previous, [group.signature]: "known" }));
    toast({
      text: "已创建 Issue 并标记为已知问题",
      delayCommit: ({ keepalive }) => createCrashIssue(api, group.signature, keepalive).then(refresh).finally(() => clear(group.signature)),
      undo: () => clear(group.signature),
      onCommitError: error => {
        clear(group.signature);
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

export function formatCount(value: number): string {
  return value.toLocaleString("zh-CN");
}

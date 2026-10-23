import { useCallback, useMemo } from "react";
import { createActionUndo } from "../../api/action-undo";
import { useQueryClient } from "@tanstack/react-query";
import { errorMessage, useAPI } from "../../api/client";
import { invalidateQueries } from "../../api/cache";
import { keys } from "../../api/keys";
import { banResultSchema, unbanResultSchema, banUndoSchema } from "../../api/users";
import { useConfirm } from "../../ui/confirm";
import { useToast } from "../../ui/toast";
import { banReasons } from "./labels";

export type BanTarget = { id: string; name: string };

// useUserActions 封装账号管理操作，封禁撤销使用服务端返回的版本和实际原因。
export function useUserActions() {
  const api = useAPI();
  const client = useQueryClient();
  const confirm = useConfirm();
  const toast = useToast();
  // 封禁会改变用户列表、统计和详情，并移除社区内容，因此也要刷新外壳的社区待审核数。
  const refresh = useCallback(() => invalidateQueries(client, keys.page("users"), keys.page("community"), keys.shell), [client]);

  const runBan = useCallback(async (id: string, reason: string) => {
    try {
      return banResultSchema.parse(await api.action({ action: "ban_user", id, reason }));
    } finally {
      await refresh();
    }
  }, [api, refresh]);
  const runUnban = useCallback(async (id: string) => {
    try {
      return unbanResultSchema.parse(await api.action({ action: "unban_user", id }));
    } finally {
      await refresh();
    }
  }, [api, refresh]);

  // 不从旧页面猜测封禁原因；缺少有效版本的旧响应不提供撤销。
  const undoFrom = useCallback((value: unknown, id: string, banned: boolean) => {
    return createActionUndo(banUndoSchema, value, undo => undo.id === id && undo.value.banned === banned, async undo => {
      try {
        await api.action(undo);
      } finally {
        await refresh();
      }
    });
  }, [api, refresh]);

  const ban = useCallback(async (user: BanTarget) => {
    const reason = await confirm({ title: `封禁 ${user.name}？`, description: "账号将无法登录，已发布的社区作品会同时下架。可随时解除封禁。", okLabel: "封禁", reasons: banReasons });
    if (reason === null) return;
    try {
      const result = await runBan(user.id, reason);
      const removed = result.removed > 0 ? `，下架 ${result.removed} 件作品` : "";
      toast({ text: `${user.name} 已封禁：${reason}${removed}`, undo: undoFrom(result.undo, user.id, false) });
    } catch (error) {
      toast(`操作失败：${errorMessage(error)}`);
    }
  }, [confirm, runBan, toast, undoFrom]);

  const unban = useCallback(async (user: BanTarget) => {
    try {
      const result = await runUnban(user.id);
      toast({ text: `${user.name} 已解除封禁`, undo: undoFrom(result.undo, user.id, true) });
    } catch (error) {
      toast(`操作失败：${errorMessage(error)}`);
    }
  }, [runUnban, toast, undoFrom]);

  const revokeSession = useCallback(async (userID: string, sessionID: string) => {
    const ok = await confirm({ title: "下线这台设备？", description: "该设备的登录凭据立即失效，需要重新登录。", okLabel: "下线" });
    if (ok === null) return;
    try {
      await api.action({ action: "revoke_session", id: sessionID, user_id: userID });
      toast("已下线该设备");
    } catch (error) {
      toast(`操作失败：${errorMessage(error)}`);
    } finally {
      await refresh();
    }
  }, [api, confirm, refresh, toast]);

  const revokeAll = useCallback(async (user: BanTarget) => {
    const ok = await confirm({ title: `下线 ${user.name} 的全部设备？`, description: "所有登录凭据立即失效，账号本身不受影响，可以重新登录。", okLabel: "全部下线" });
    if (ok === null) return;
    try {
      const result = await api.action({ action: "revoke_sessions", id: user.id });
      toast(`已下线 ${result.affected} 个会话`);
    } catch (error) {
      toast(`操作失败：${errorMessage(error)}`);
    } finally {
      await refresh();
    }
  }, [api, confirm, refresh, toast]);

  // Stable identity, so table columns built from these callbacks stay memoized.
  return useMemo(() => ({ ban, unban, revokeSession, revokeAll }), [ban, unban, revokeSession, revokeAll]);
}

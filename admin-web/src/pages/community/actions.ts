import { useCallback } from "react";
import { z } from "zod";
import { useQueryClient } from "@tanstack/react-query";
import { errorMessage, isAPIError, useAPI } from "../../api/client";
import { keys } from "../../api/keys";
import type { CandidateCategory, Moderation, Section } from "../../api/community";
import { candidateCategoryLabels, categoryUndoSchema, moderationSchema, removeReasons, sections } from "../../api/community";
import { useConfirm } from "../../ui/confirm";
import { useToast } from "../../ui/toast";

// 审核冲突提示同时适用于首次操作和撤销。
function failure(error: unknown): string {
  return isAPIError(error, "conflict") ? "该内容刚被其他审核员处理或被作者修改，未做改动，请查看最新内容后重试。" : errorMessage(error);
}

// Target is what an action needs to know about an item, from a list row or a detail. updated_at is the version the moderator is looking at (only editable sections have one) and previous_moderation is a removed item's restore state. created_at tells the row apart from one the author published under the same id after deleting it.
export type Target = { section: Section; id: string; name: string; moderation: Moderation; moderation_reason?: string | null; previous_moderation?: Moderation | null; created_at?: string; updated_at?: string | null; edit_token?: string };

// approveValue pins an approval to the state, row and version the moderator saw, so a stale card cannot republish a removed item or publish content nobody reviewed.
function approveValue(target: Target) {
  return { from: target.moderation, ...(target.created_at ? { created_at: target.created_at } : {}), ...(target.updated_at ? { updated_at: target.updated_at } : {}), ...(target.edit_token ? { edit_token: target.edit_token } : {}) };
}

const moderationUndoSchema = z.object({
  action: z.literal("undo_moderation"), section: z.enum(sections), id: z.string(),
  value: z.object({ token: z.string().regex(/^[a-f0-9]{32}$/), moderation: moderationSchema, previous: z.enum(["pending", "approved"]).nullable(), reason: z.string().nullable() }),
});

// useModeration 封装审核确认与提示；审核和分类撤销都使用服务端返回的真实前态及版本。
export function useModeration() {
  const api = useAPI();
  const client = useQueryClient();
  const confirm = useConfirm();
  const toast = useToast();

  const refresh = useCallback(() => Promise.all([
    client.invalidateQueries({ queryKey: keys.page("community") }),
    client.invalidateQueries({ queryKey: keys.shell }),
  ]), [client]);

  const run = useCallback(async (body: Parameters<typeof api.action>[0]) => {
    // 首次操作和撤销共用审核专用错误提示。
    try {
      return await api.action(body);
    } catch (error) {
      throw new Error(failure(error));
    } finally {
      await refresh();
    }
  }, [api, refresh]);

  // 缺少有效撤销信息的旧副本不显示按钮；不从旧页面猜测操作前状态。
  const undoFrom = useCallback((result: Awaited<ReturnType<typeof run>>, target: Target) => {
    const parsed = moderationUndoSchema.safeParse(result.undo);
    if (!parsed.success || parsed.data.section !== target.section || parsed.data.id !== target.id) return undefined;
    return async () => { await run(parsed.data); };
  }, [run]);

  const approve = useCallback(async (target: Target) => {
    if (target.moderation === "approved") return;
    try {
      const result = await run({ action: "approve_content", section: target.section, id: target.id, value: approveValue(target) });
      toast({ text: `「${target.name}」已上架`, undo: undoFrom(result, target) });
    } catch (error) {
      toast(`操作失败：${errorMessage(error)}`);
      return;
    }
  }, [run, toast, undoFrom]);

  const remove = useCallback(async (target: Target) => {
    const verb = target.moderation === "approved" ? "下架" : "驳回";
    const reason = await confirm({ title: `${verb}「${target.name}」？`, description: "内容会从公开列表隐藏（作者本人仍可见），可随时恢复；原因记入审计日志。", okLabel: verb, reasons: removeReasons });
    if (reason === null) return;
    try {
      const result = await run({ action: "remove_content", section: target.section, id: target.id, reason });
      toast({ text: `「${target.name}」已${verb}：${reason}`, undo: undoFrom(result, target) });
    } catch (error) {
      toast(`操作失败：${errorMessage(error)}`);
      return;
    }
  }, [confirm, run, toast, undoFrom]);

  const restore = useCallback(async (target: Target) => {
    try {
      const result = await run({ action: "restore_content", section: target.section, id: target.id });
      toast({ text: `「${target.name}」已恢复`, undo: undoFrom(result, target) });
    } catch (error) {
      toast(`操作失败：${errorMessage(error)}`);
      return;
    }
  }, [run, toast, undoFrom]);

  const setCategory = useCallback(async (target: Target, from: CandidateCategory, to: CandidateCategory) => {
    if (from === to) return;
    const action = target.section === "skins" ? "set_skin_category" : "set_candidate_skin_category";
    try {
      const result = await run({ action, section: target.section, id: target.id, value: { category: to, expected: from } });
      const parsed = categoryUndoSchema.safeParse(result.undo);
      const undo = parsed.success && parsed.data.action === action && parsed.data.section === target.section && parsed.data.id === target.id ? async () => { await run(parsed.data); } : undefined;
      toast({ text: `「${target.name}」的分类已改为「${candidateCategoryLabels[to]}」`, undo });
    } catch (error) {
      toast(`操作失败：${errorMessage(error)}`);
      return;
    }
  }, [run, toast]);

  const deleteItem = useCallback(async (target: Target) => {
    const actions: Partial<Record<Section, string>> = { skins: "delete_skin", "candidate-skins": "delete_candidate_skin", plugins: "delete_plugin" };
    const action = actions[target.section];
    if (!action) return false;
    const reason = await confirm({ title: `删除「${target.name}」？`, description: "作品及其下载、评分、收藏记录将永久删除。确认后有 4 秒可以撤销。", okLabel: "删除" });
    if (reason === null) return false;
    toast({ text: `即将删除「${target.name}」`, delayCommit: async ({ keepalive }) => {
      try {
        await api.action({ action, section: target.section, id: target.id }, { keepalive });
      } finally {
        await refresh();
        await client.invalidateQueries({ queryKey: keys.page("users") });
      }
    } });
    return true;
  }, [api, client, confirm, refresh, toast]);

  return { approve, remove, restore, setCategory, deleteItem };
}

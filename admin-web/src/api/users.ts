import { z } from "zod";

// GET /api/users: one page of 50 accounts. contact is masked by the server; role is an admin role key when a verified email belongs to an enabled admin member, otherwise "user".
export const userRowSchema = z.object({
  id: z.string(),
  display_name: z.string(),
  created_at: z.string(),
  devices: z.number().int().nonnegative(),
  last_active: z.string().nullable(),
  contact: z.string(),
  contact_kind: z.enum(["email", "phone", ""]),
  role: z.string(),
  banned: z.boolean(),
  banned_at: z.string().nullable(),
  ban_reason: z.string(),
});
export type UserRow = z.infer<typeof userRowSchema>;
export const usersSchema = z.object({ items: z.array(userRowSchema), page: z.number(), total: z.number(), has_more: z.boolean() });

// GET /api/users/stats: totals for the stat tiles and per-role counts for the filter chips.
export const userStatsSchema = z.object({
  total: z.number(),
  new_7d: z.number(),
  sync_ratio: z.number(),
  banned: z.number(),
  roles: z.record(z.string(), z.number()),
});
export type UserStats = z.infer<typeof userStatsSchema>;

const userSessionSchema = z.object({
  id: z.string(),
  created_at: z.string(),
  expires_at: z.string(),
  last_active: z.string(),
  user_agent: z.string(),
  status: z.enum(["active", "expired", "revoked"]),
});
export type UserSession = z.infer<typeof userSessionSchema>;

export const workSections = ["skins", "candidate-skins", "plugins", "dictionaries", "replies", "phrases"] as const;
const userWorkSchema = z.object({
  section: z.enum(workSections),
  id: z.string(),
  name: z.string(),
  moderation: z.enum(["pending", "approved", "removed"]),
  moderation_reason: z.string(),
  created_at: z.string(),
  downloads: z.number(),
  saves: z.number(),
});
export type UserWork = z.infer<typeof userWorkSchema>;

const userHistorySchema = z.object({
  id: z.number(),
  action: z.string(),
  actor: z.string(),
  detail: z.record(z.string(), z.unknown()),
  created_at: z.string(),
});
export type UserHistory = z.infer<typeof userHistorySchema>;

// GET /api/users/{id}: profile, masked contact, ban state, the latest 50 sessions, the latest 20 community works and the admin actions taken on the account.
export const userDetailSchema = z.object({
  id: z.string(),
  display_name: z.string(),
  created_at: z.string(),
  providers: z.array(z.string()),
  role: z.string(),
  contact: z.string(),
  contact_kind: z.enum(["email", "phone", ""]),
  banned: z.boolean(),
  banned_at: z.string().nullable(),
  ban_reason: z.string(),
  banned_by: z.string(),
  sync: z.boolean(),
  last_active: z.string().nullable(),
  active_sessions: z.number(),
  total_sessions: z.number(),
  sessions: z.array(userSessionSchema),
  works: z.array(userWorkSchema),
  history: z.array(userHistorySchema),
});
export type UserDetail = z.infer<typeof userDetailSchema>;

// ban_user answers how many sessions it revoked and how many community items it removed; unban_user how many it restored.
export const banResultSchema = z.looseObject({ ok: z.literal(true), affected: z.number(), sessions: z.number(), removed: z.number() });
export const unbanResultSchema = z.looseObject({ ok: z.literal(true), affected: z.number(), restored: z.number() });

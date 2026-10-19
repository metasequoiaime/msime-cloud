import { z } from "zod";

// Issue triage: GET /api/issues, GET /api/issues/{owner}/{repo}/{n} and POST /api/issues/actions (internal/server/admin_issues.go).

export const issueStates = ["new", "triaged", "done", "dup"] as const;
export type IssueState = (typeof issueStates)[number];

// IssueStateFilter is the ?state= of GET /api/issues: a console state or one of the open / closed / all groups.
export type IssueStateFilter = "open" | "new" | "triaged" | "closed" | "all";

export const issueRowSchema = z.object({
  repo: z.string(),
  number: z.number().int().positive(),
  title: z.string(),
  author: z.string(),
  url: z.string(),
  state: z.enum(issueStates),
  // platform is a configured platform id, or "" when the issue matches none.
  platform: z.string(),
  // kind is bug, idea or docs from the labels, or "".
  kind: z.string(),
  labels: z.array(z.string()),
  assignees: z.array(z.string()),
  comments: z.number().int().nonnegative(),
  created_at: z.string(),
  updated_at: z.string(),
  closed_at: z.string().nullable(),
});
export type IssueRow = z.infer<typeof issueRowSchema>;

export const issuePlatformSchema = z.object({ id: z.string(), name: z.string(), label: z.string(), assignee: z.string() });
export type IssuePlatform = z.infer<typeof issuePlatformSchema>;

export const issuesSchema = z.object({
  items: z.array(issueRowSchema),
  page: z.number().int().positive(),
  page_size: z.number().int().positive(),
  total: z.number().int().nonnegative(),
  has_more: z.boolean(),
  repos: z.array(z.string()),
  platforms: z.array(issuePlatformSchema),
  // unavailable lists the configured repositories GitHub could not be read for.
  unavailable: z.array(z.string()),
  platform_counts: z.record(z.string(), z.number().int().nonnegative()),
  state_counts: z.record(z.string(), z.number().int().nonnegative()),
  stats: z.object({
    pending: z.number().int().nonnegative(),
    triaged: z.number().int().nonnegative(),
    new_this_week: z.number().int().nonnegative(),
    first_response_hours: z.number().nonnegative().nullable(),
    first_response_samples: z.number().int().nonnegative(),
  }),
});
export type IssuesList = z.infer<typeof issuesSchema>;

export const issueEventSchema = z.object({ kind: z.string(), actor: z.string(), text: z.string(), at: z.string() });
export type IssueEvent = z.infer<typeof issueEventSchema>;

export const issueDetailSchema = z.object({
  issue: issueRowSchema.extend({ body: z.string() }),
  timeline: z.array(issueEventSchema),
  // timeline_truncated is true when the timeline has more than one page and only the newest page is shown.
  timeline_truncated: z.boolean(),
  similar: z.array(z.object({ repo: z.string(), number: z.number().int().positive(), title: z.string(), state: z.enum(issueStates), url: z.string() })),
  // platform_assignee is who triage assigns, "" when the platform has none or the issue has no platform.
  platform_assignee: z.string(),
});
export type IssueDetail = z.infer<typeof issueDetailSchema>;

export type IssueAction = "triage" | "untriage" | "mark_dup" | "close" | "reopen" | "comment";
// 撤销时保留操作前已有的平台负责人或重复标签，分别只用于 untriage 和 reopen。
const issueRefSchema = z.object({ repo: z.string(), n: z.number().int().positive(), keep_assignee: z.boolean().optional(), keep_duplicate: z.boolean().optional() });
export type IssueRef = z.infer<typeof issueRefSchema>;

export const issueActionResultSchema = z.object({
  ok: z.literal(true),
  affected: z.number().int().nonnegative(),
  failed: z.array(z.object({ repo: z.string(), n: z.number().int(), code: z.string() })),
  // 旧服务端缺少 undo 时不根据页面缓存猜测撤销参数。
  undo: z.object({ action: z.enum(["untriage", "reopen"]), items: z.array(issueRefSchema).min(1) }).nullish(),
});
export type IssueActionResult = z.infer<typeof issueActionResultSchema>;

export function issueRef(row: { repo: string; number: number }): IssueRef {
  return { repo: row.repo, n: row.number };
}

// issueKey is the id global search and notifications pass as ?focus=, e.g. "metasequoiaime/msime#12".
export function issueKey(row: { repo: string; number: number }): string {
  return `${row.repo}#${row.number}`;
}

// issuePath is the detail path of an issue below /api/, with each segment encoded.
export function issuePath(ref: IssueRef): string {
  const [owner, name] = ref.repo.split("/");
  return `issues/${encodeURIComponent(owner)}/${encodeURIComponent(name)}/${ref.n}`;
}

export function parseIssueKey(key: string): IssueRef | null {
  const match = /^([^/#\s]+\/[^/#\s]+)#([1-9][0-9]*)$/.exec(key);
  return match ? { repo: match[1], n: Number(match[2]) } : null;
}

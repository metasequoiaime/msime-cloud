import { createContext, useContext, useMemo } from "react";
import { z } from "zod";

export type Method = "GET" | "POST" | "PATCH" | "DELETE";

// Server errors arrive as {"error":{"code","message"}} from fail() or as {"error":"code"} from newer handlers; both carry a stable machine code.
const errorBodySchema = z.looseObject({ error: z.union([z.string(), z.object({ code: z.string(), message: z.string().optional() })]) });

// codeMessages is the single place where server error codes become console copy; pages show errorMessage(error) and only override a code when their context needs different wording.
const codeMessages: Record<string, string> = {
  // Session, permissions and request limits.
  unauthorized: "登录已失效或凭据无效，请重新登录。",
  permission_denied: "当前角色没有执行此操作的权限。",
  owner_required: "只有配置中的所有者可以执行此操作。",
  origin_denied: "请求来源被拒绝，请从管理后台域名访问。",
  origin_required: "请求来源与 Google 登录回调地址不一致：后台的访问地址（协议和域名）必须与 admin.google.redirect_uri 相同。",
  admin_auth_unavailable: "暂时无法校验登录状态，请检查服务和数据库状态后重试。",
  rate_limit_exceeded: "请求过于频繁，请稍后重试。",
  not_implemented: "该功能的服务端接口尚未实现。",
  // Generic record and validation errors.
  exists: "记录已存在。",
  protected: "该项受保护，不能修改。",
  conflict: "数据已被其他人修改，请刷新后重试。",
  not_found: "记录不存在，可能已被删除，请刷新列表。",
  invalid_request: "参数无效，请检查输入。",
  invalid_json: "请求内容不是有效的 JSON，或超过大小限制。",
  unknown_field: "请求包含服务端不认识的字段，请检查前后端版本是否一致。",
  invalid_action: "服务端不支持这个操作，请检查前后端版本是否一致。",
  invalid_id: "记录 ID 无效，请刷新后重试。",
  invalid_ids: "选择的记录无效或超过 100 条，请刷新后重试。",
  invalid_user_id: "用户 ID 无效，请刷新后重试。",
  invalid_section: "内容分类无效，请刷新后重试。",
  invalid_reason: "原因不能为空，且不超过 500 字。",
  invalid_value: "提交的内容无效或过长，请检查后重试。",
  invalid_body: "正文过长或包含不支持的字符。",
  invalid_title: "标题不能为空，最多 200 字，且不能包含控制字符。",
  invalid_description: "描述过长或包含不支持的字符。",
  invalid_service: "服务名无效。",
  invalid_dictionary_path: "词库文件路径无效。",
  dictionary_readonly: "这个文件按词库仓库约定只能查看。",
  invalid_dictionary_changes: "词库变更格式无效，请刷新文件后重试。",
  invalid_dictionary_entry: "词条格式、读音或权重不符合词库规则。",
  dictionary_changed: "词库文件已经更新，请刷新后重新编辑。",
  dictionary_duplicate: "变更会产生重复词条，请检查内容。",
  dictionary_no_changes: "没有检测到词条变化。",
  dictionary_uncertain: "无法确认 GitHub 的写入结果，请先检查仓库分支。",
  dictionary_not_recorded: "Pull Request 已创建，但操作记录暂未写入。",
  dictionary_encoding: "文件编码无法预览，请在 GitHub 中查看。",
  // Members and roles.
  admin_exists: "该邮箱已经是管理员。",
  admin_limit: "最多只能添加 100 名管理员。",
  protected_owner: "配置中的所有者（admin.owners）不能在后台修改。",
  invalid_admin: "邮箱格式不正确。",
  invalid_role: "角色无效，请刷新后重试。",
  invalid_permission: "权限项无效，请刷新后重试。",
  // Users and moderation.
  account_banned: "该账号已被封禁。",
  already_banned: "该账号已被封禁。",
  not_banned: "该账号未被封禁。",
  owner_banned: "作者账号已被封禁，解封账号后内容才会恢复。",
  not_removed: "该内容已不在下架状态，请刷新后重试。",
  blocked_word: "内容命中敏感词，已被拦截。",
  blocked_content: "内容命中敏感词，已被拦截。",
  screening_unavailable: "敏感词检查暂时不可用，请稍后重试。",
  // Sensitive words.
  invalid_pattern: "规则无效：正则需符合 RE2 语法且不能匹配空文本，{n} 这类计数重复展开后不超过 100 步，长度不超过 200 个字符，不能含换行等控制字符。",
  invalid_category: "请选择有效的分类。",
  invalid_level: "请选择有效的处理方式。",
  // Notices.
  telegram_disabled: "服务端未配置 Telegram 渠道（admin.telegram），请取消勾选 Telegram。",
  telegram_failed: "Telegram 推送失败，公告没有发布，请稍后重试。",
  not_draft: "这条公告已经发布或归档，不能再编辑，请刷新列表。",
  already_archived: "这条公告已经归档。",
  invalid_targets: "请选择投放范围。",
  invalid_channels: "请至少选择一个渠道。",
  // Site settings.
  invalid_lanzou_url: "链接无效：需要以 https:// 开头、带主机名、不含账号密码，最长 512 字节。",
  // Service incidents.
  already_resolved: "该故障已经恢复。",
  // GitHub integration shared by dictionary PRs, issues, releases and crash issues.
  github_disabled: "GitHub 集成未配置。",
  github_unavailable: "GitHub 暂时无法访问，请稍后重试。",
  github_rejected: "GitHub 拒绝了请求，请检查 GitHub App 的安装范围和权限。",
  github_repo_not_found: "GitHub App 无法访问该仓库，请检查仓库名和安装范围。",
  github_error: "GitHub 拒绝了这次操作，请在 GitHub 上查看状态。",
  github_uncertain: "无法确认 GitHub 上的操作结果，请先在 GitHub 上核对再重试。",
  // Dictionary pull requests.
  pr_changed: "PR 有了新的提交，请刷新后重新核对词条。",
  not_open: "这个 PR 已经不是待审核状态，请刷新列表。",
  unexpected_files: "这个 PR 还改动了词库投稿文件之外的文件，不能在后台精简或通过，请在 GitHub 上处理。",
  not_mergeable: "GitHub 暂时无法合并这个 PR（可能有冲突或检查未通过）。",
  invalid_keep: "勾选的词条无效，请刷新后重试。",
  invalid_head_sha: "缺少 PR 版本信息，请刷新后重试。",
  // Releases.
  no_workflow: "该平台没有配置发布流水线（admin.github.platforms[].release_workflow）。",
  workflow_not_found: "GitHub 上找不到这个平台的发布流水线文件。",
  workflow_rejected: "发布流水线拒绝了这次触发：需要 workflow_dispatch 触发器和 version 输入。",
  invalid_version: "版本号格式不正确，例如 v0.5.5。",
  invalid_tag: "tag 与平台的前缀不匹配。",
  not_published: "草稿还没有发布，不能撤回。",
  already_withdrawn: "该版本已经撤回。",
  release_rejected: "GitHub 拒绝了这次修改。",
  // Crash group issues.
  issue_exists: "这个分组已经建过 Issue。",
  issue_in_progress: "另一个请求正在为这个分组建 Issue，请稍后刷新。",
  platform_not_configured: "该平台未在 admin.github.platforms 中配置仓库，无法建 Issue。",
  issue_not_recorded: "Issue 已在 GitHub 创建，但未能写回分组，请勿重复创建，稍后将分组标记为已知。",
  // 服务日志。
  logs_disabled: "服务端未配置日志来源（admin.logs.loki_url）。",
  logs_unavailable: "暂时无法读取日志：服务端连不上 Loki 或 Loki 返回了错误。",
  too_many_streams: "同时打开的日志流过多（每人最多 2 个），请关闭其他日志页面后重试。",
  invalid_pod: "副本名无效，请刷新后重试。",
  invalid_since: "时间范围无效，应在 1 分钟到 24 小时之间。",
  invalid_limit: "行数无效，应在 1 到 2000 之间。",
  invalid_query: "搜索内容无效：最多 200 字节，不能包含换行等控制字符。",
  invalid_cursor: "续传位置无效，请刷新页面。",
  invalid_backfill: "回填行数无效，应在 0 到 1000 之间。",
};

// codeMessage returns the console copy for a server error code that arrives as data rather than as a failed request, such as a per-platform error inside a listing.
export function codeMessage(code: string): string | undefined {
  return Object.hasOwn(codeMessages, code) ? codeMessages[code] : undefined;
}

function statusMessage(status: number): string {
  if (status === 401) return codeMessages.unauthorized;
  if (status === 403) return "没有执行此操作的权限，或该账号受到保护。";
  if (status === 404) return codeMessages.not_found;
  if (status === 409) return "操作冲突：记录已存在或已被修改，请刷新后重试。";
  if (status === 400 || status === 422) return codeMessages.invalid_request;
  if (status === 429) return codeMessages.rate_limit_exceeded;
  return `请求失败 (${status})，请检查服务和数据库状态。`;
}

export class APIError extends Error {
  readonly status: number;
  readonly code: string;
  // details is the rest of the error body, for the few errors that carry data the caller must not lose (issue_not_recorded's issue_url).
  readonly details: Readonly<Record<string, unknown>>;
  constructor(status: number, code: string, details: Readonly<Record<string, unknown>> = {}) {
    super(codeMessage(code) ?? statusMessage(status));
    this.status = status;
    this.code = code;
    this.details = details;
  }
}

export type RequestOptions = {
  method?: Method;
  body?: unknown;
  signal?: AbortSignal;
  token?: string;
  // keepalive lets a request outlive the page; use it for delayed commits flushed on pagehide.
  keepalive?: boolean;
};

// requestAPI calls /api/<path> and validates the JSON response with the given zod schema. A body without an explicit method is sent as POST.
export async function requestAPI<S extends z.ZodType>(path: string, schema: S, options: RequestOptions = {}): Promise<z.infer<S>> {
  const { body, signal, token, keepalive } = options;
  const method = options.method ?? (body === undefined ? "GET" : "POST");
  const response = await fetch(`/api/${path.replace(/^\/+/, "")}`, {
    method, signal, keepalive, credentials: "same-origin",
    headers: { Accept: "application/json", ...(token ? { Authorization: `Bearer ${token}` } : {}), ...(body === undefined ? {} : { "Content-Type": "application/json" }) },
    ...(body === undefined ? {} : { body: JSON.stringify(body) }),
  });
  if (!response.ok) {
    let code = "";
    let details: Record<string, unknown> = {};
    const text = await response.text();
    if (text) {
      try {
        const parsed = errorBodySchema.safeParse(JSON.parse(text));
        if (parsed.success) {
          code = typeof parsed.data.error === "string" ? parsed.data.error : parsed.data.error.code;
          details = parsed.data;
        }
      } catch {
        code = "";
      }
    }
    throw new APIError(response.status, code, details);
  }
  const text = await response.text();
  return schema.parse(text ? JSON.parse(text) : null);
}

export function errorMessage(error: unknown): string {
  if (error instanceof z.ZodError) return "服务返回的数据格式不正确，请检查前后端版本是否一致。";
  if (error instanceof DOMException && error.name === "AbortError") return "请求已取消。";
  return error instanceof Error ? error.message : "请求失败，请重试。";
}

export function isAPIError(error: unknown, code: string): boolean {
  return error instanceof APIError && error.code === code;
}

// isGithubDisabled marks the explicit 未配置 state of pages backed by the optional admin.github config.
export function isGithubDisabled(error: unknown): boolean {
  return error instanceof APIError && error.status === 404 && error.code === "github_disabled";
}

// Loose so extra fields that an action adds to the response (for example the id of a created row) reach the caller instead of being stripped.
export const actionResultSchema = z.looseObject({ ok: z.literal(true), affected: z.number() });
export type ActionResult = z.infer<typeof actionResultSchema>;

// ActionRequest is the body of POST /api/actions; the server rejects unknown fields.
export type ActionRequest = {
  action: string;
  id?: string;
  user_id?: string;
  ids?: string[];
  reason?: string;
  section?: string;
  value?: unknown;
};

type Credentials = { token: string; onUnauthorized: () => void };
export const APICredentialsContext = createContext<Credentials | null>(null);

type CallOptions = Omit<RequestOptions, "method" | "token" | "body">;

export type API = {
  get<S extends z.ZodType>(path: string, schema: S, options?: CallOptions): Promise<z.infer<S>>;
  post<S extends z.ZodType>(path: string, schema: S, body?: unknown, options?: CallOptions): Promise<z.infer<S>>;
  patch<S extends z.ZodType>(path: string, schema: S, body?: unknown, options?: CallOptions): Promise<z.infer<S>>;
  delete<S extends z.ZodType>(path: string, schema: S, options?: CallOptions): Promise<z.infer<S>>;
  action(body: ActionRequest, options?: CallOptions): Promise<ActionResult>;
};

// useAPI binds requestAPI to the signed-in admin (cookie session or in-memory legacy token) and signs out on 401.
export function useAPI(): API {
  const credentials = useContext(APICredentialsContext);
  if (!credentials) throw new Error("AuthProvider required");
  const { token, onUnauthorized } = credentials;
  return useMemo(() => {
    const call = async <S extends z.ZodType>(path: string, schema: S, options: RequestOptions): Promise<z.infer<S>> => {
      try {
        return await requestAPI(path, schema, { ...options, token });
      } catch (error) {
        if (error instanceof APIError && error.status === 401) onUnauthorized();
        throw error;
      }
    };
    return {
      get: (path, schema, options) => call(path, schema, { ...options, method: "GET" }),
      post: (path, schema, body, options) => call(path, schema, { ...options, method: "POST", body: body ?? {} }),
      patch: (path, schema, body, options) => call(path, schema, { ...options, method: "PATCH", body: body ?? {} }),
      delete: (path, schema, options) => call(path, schema, { ...options, method: "DELETE" }),
      action: (body, options) => call("actions", actionResultSchema, { ...options, method: "POST", body }),
    };
  }, [token, onUnauthorized]);
}

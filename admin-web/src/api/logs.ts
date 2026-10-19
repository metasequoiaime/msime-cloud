import { z } from "zod";
import { APIError } from "./client";

// 一行日志。id 用于重连去重，旧副本可省略；ts 是 Loki 的纳秒时间戳（十进制字符串，也是续传游标）；time 和 stream 来自 CRI 前缀；level 是解析出的 slog 级别，无法识别时为空；message 已去掉 CRI 前缀和 slog 的时间与级别。
export const logLineSchema = z.object({
  id: z.string().optional(),
  ts: z.string(),
  time: z.string(),
  pod: z.string(),
  stream: z.string(),
  level: z.string(),
  message: z.string(),
});
export type LogLine = z.infer<typeof logLineSchema>;

// GET /api/logs?since=&limit=&pod=&level=&q=（需要 view_logs）：窗口内最新的 limit 行，按时间升序。
export const logsSchema = z.object({
  lines: z.array(logLineSchema),
  pods: z.array(z.string()),
  truncated: z.boolean(),
  cursor: z.string(),
});

export const streamLinesSchema = z.object({ lines: z.array(logLineSchema) });
export const streamPodsSchema = z.object({ pods: z.array(z.string()) });
export const streamGapSchema = z.object({ from: z.string(), to: z.string() });

export const logLevels = ["INFO", "WARN", "ERROR"] as const;
export type LogLevel = (typeof logLevels)[number];

// compareTs 比较两个纳秒时间戳字符串；位数不同时位数多的更大。
export function compareTs(a: string, b: string): number {
  if (a.length !== b.length) return a.length - b.length;
  return a < b ? -1 : a > b ? 1 : 0;
}

export type StreamEvent = { event: string; id: string; data: string };

// parseSSE 把一段 text/event-stream 文本拆成完整的事件，返回事件和尚未结束的剩余部分。注释行（心跳）和 retry 字段被忽略。
export function parseSSE(buffer: string): { events: StreamEvent[]; rest: string } {
  const events: StreamEvent[] = [];
  const normalized = buffer.replace(/\r\n?/g, "\n");
  const frames = normalized.split("\n\n");
  const rest = frames.pop() ?? "";
  for (const frame of frames) {
    let event = "message";
    let id = "";
    const data: string[] = [];
    for (const line of frame.split("\n")) {
      if (!line || line.startsWith(":")) continue;
      const colon = line.indexOf(":");
      const field = colon < 0 ? line : line.slice(0, colon);
      const value = colon < 0 ? "" : line.slice(colon + 1).replace(/^ /, "");
      if (field === "event") event = value;
      else if (field === "id") id = value;
      else if (field === "data") data.push(value);
    }
    if (data.length) events.push({ event, id, data: data.join("\n") });
  }
  return { events, rest };
}

// openLogStream 用 fetch 读取 GET /api/logs/stream：EventSource 不能带 Authorization 请求头，而管理员密钥登录只在内存里保存令牌。onEvent 收到每个完整事件；流正常结束时 resolve，HTTP 错误抛出 APIError。
export async function openLogStream(query: URLSearchParams, options: { token: string; signal: AbortSignal; onEvent: (event: StreamEvent) => void }): Promise<void> {
  const response = await fetch(`/api/logs/stream?${query.toString()}`, {
    signal: options.signal,
    credentials: "same-origin",
    headers: { Accept: "text/event-stream", ...(options.token ? { Authorization: `Bearer ${options.token}` } : {}) },
  });
  if (!response.ok || !response.body) {
    let code = "";
    try {
      const body = await response.json() as { error?: { code?: string } | string };
      code = typeof body.error === "string" ? body.error : body.error?.code ?? "";
    } catch {
      code = "";
    }
    throw new APIError(response.status, code);
  }
  const reader = response.body.pipeThrough(new TextDecoderStream()).getReader();
  let buffer = "";
  for (;;) {
    const { value, done } = await reader.read();
    if (done) return;
    buffer += value;
    const parsed = parseSSE(buffer);
    buffer = parsed.rest;
    for (const event of parsed.events) options.onEvent(event);
  }
}

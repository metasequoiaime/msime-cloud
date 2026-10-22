import { z } from "zod";

// 后台撤销版本使用服务端生成的 32 位小写十六进制标识。
export const actionUndoTokenSchema = z.string().regex(/^[a-f0-9]{32}$/);

// 校验服务端返回的撤销快照，并捕获解析后的副本；回调只在提示条真正触发时提交。
export function createActionUndo<S extends z.ZodType>(schema: S, raw: unknown, matches: (value: z.output<S>) => boolean, run: (value: z.output<S>) => Promise<unknown>): (() => Promise<void>) | undefined {
  const parsed = schema.safeParse(raw);
  if (!parsed.success || !matches(parsed.data)) return undefined;
  const value = parsed.data;
  return async () => { await run(value); };
}

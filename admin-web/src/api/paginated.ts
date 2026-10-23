import { z } from "zod";

// 共用按页列表的响应结构，保留行 schema 的校验、转换和类型推断。
// 默认仅要求有限数字；需要整数或正数的接口在调用处扩展约束。
export function paginatedSchema<T extends z.ZodType>(item: T) {
  return z.object({ items: z.array(item), page: z.number(), total: z.number(), has_more: z.boolean() });
}

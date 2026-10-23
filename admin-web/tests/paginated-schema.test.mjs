import assert from 'node:assert/strict';
import { test } from 'node:test';
import { z } from 'zod';
import '../src/zod-config.ts';
import { paginatedSchema } from '../src/api/paginated.ts';
import { listSchema, itemSchema } from '../src/api/community.ts';
import { feedbackListSchema, feedbackRowSchema } from '../src/api/feedback.ts';
import { auditListSchema, auditEntrySchema } from '../src/api/perm.ts';
import { usersSchema, userRowSchema } from '../src/api/users.ts';
import { issuesSchema } from '../src/api/issues.ts';

test('分页解析沿用行 schema 的转换与未知字段清理', () => {
  const schema = paginatedSchema(z.object({ id: z.string(), count: z.string().transform(Number) }));
  const input = { items: [{ id: 'synthetic-row', count: '3', extra: true }], page: 1, total: 1, has_more: false, extra: true };
  assert.deepEqual(schema.parse(input), { items: [{ id: 'synthetic-row', count: 3 }], page: 1, total: 1, has_more: false });
  assert.equal(input.items[0].count, '3');
  assert.equal(input.extra, true);
});

test('普通分页保留有限数字边界，不新增整数或正数限制', () => {
  const schema = paginatedSchema(z.string());
  for (const value of [-12, -0, 0, 1, 2.5, Number.MAX_VALUE]) {
    assert.deepEqual(schema.parse({ items: [], page: value, total: value, has_more: false }), { items: [], page: value, total: value, has_more: false });
  }
  for (const field of ['page', 'total']) {
    for (const value of [undefined, null, '1', false, NaN, Infinity, -Infinity, {}, []]) {
      const result = schema.safeParse({ items: [], page: 1, total: 0, has_more: false, [field]: value });
      assert.equal(result.success, false, `${field}: ${String(value)}`);
      assert.deepEqual(result.error.issues[0].path, [field]);
    }
  }
  for (const value of [undefined, null, 0, 'false']) {
    assert.equal(schema.safeParse({ items: [], page: 1, total: 0, has_more: value }).success, false);
  }
  assert.equal(schema.safeParse({ items: [1], page: 1, total: 1, has_more: true }).success, false);
  assert.equal(schema.safeParse({ page: 1, total: 0, has_more: false }).success, false);
});

const cases = [
  ['用户', usersSchema, userRowSchema, { id: 'user-1', display_name: '合成用户', created_at: '2026-01-01T00:00:00Z', devices: 0, last_active: null, contact: 'u***@example.test', contact_kind: 'email', role: 'user', banned: false, banned_at: null, ban_reason: '' }],
  ['反馈', feedbackListSchema, feedbackRowSchema, { id: 'feedback-1', type: 'bug', text: '合成反馈', platform: 'linux', app_version: '1.0', edition: '', diagnostics: null, status: 'new', created_at: '2026-01-01T00:00:00Z', user_id: 'user-1', author: '合成用户', anonymous: false, screenshots: 0 }],
  ['社区', listSchema, itemSchema, { id: 'skin-1', name: '合成皮肤', owner_id: 'user-1', author: '合成用户', created_at: '2026-01-01T00:00:00Z', moderation: 'pending', reports: 0 }],
  ['审计', auditListSchema, auditEntrySchema, { id: 1, action: 'synthetic_action', target: 'synthetic-target', detail: null, created_at: '2026-01-01T00:00:00Z' }],
];

for (const [name, current, rowSchema, row] of cases) {
  test(`${name}列表与抽取前的分页结构保持相同解析结果和错误路径`, () => {
    const previous = z.object({ items: z.array(rowSchema), page: z.number(), total: z.number(), has_more: z.boolean() });
    const input = { items: [{ ...row, unexpected: true }], page: 1, total: 1, has_more: false, unexpected: true };
    const samples = [input, { ...input, items: [] }, { ...input, page: -1.5, total: -2.5 }, null];
    for (const field of ['items', 'page', 'total', 'has_more']) {
      for (const value of [undefined, null, 'invalid', {}, [null]]) samples.push({ ...input, [field]: value });
    }
    samples.push({ ...input, items: [{}] });
    for (const sample of samples) {
      const expected = previous.safeParse(sample);
      const actual = current.safeParse(sample);
      assert.equal(actual.success, expected.success);
      if (expected.success) assert.deepEqual(actual.data, expected.data);
      else assert.deepEqual(actual.error.issues, expected.error.issues);
    }
  });
}

test('Issue 分页保留更严格的页码、页大小和总数约束，以及列表附加信息', () => {
  const input = {
    items: [], page: 1, page_size: 50, total: 0, has_more: false,
    repos: ['example/project'], platforms: [], unavailable: [], platform_counts: {}, state_counts: {},
    stats: { pending: 0, triaged: 0, new_this_week: 0, first_response_hours: null, first_response_samples: 0 },
  };
  assert.deepEqual(issuesSchema.parse({ ...input, unexpected: true }), input);
  for (const field of ['page', 'page_size', 'total']) {
    for (const value of [undefined, null, '1', -1, 0.5, NaN, Infinity, ...(field === 'total' ? [] : [0])]) {
      assert.equal(issuesSchema.safeParse({ ...input, [field]: value }).success, false, `${field}: ${String(value)}`);
    }
  }
  assert.equal(issuesSchema.safeParse({ ...input, stats: undefined }).success, false);
});

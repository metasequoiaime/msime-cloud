import assert from 'node:assert/strict';
import { test } from 'node:test';
import '../src/zod-config.ts';
import { createActionUndo } from '../src/api/action-undo.ts';
import { categoryUndoSchema, moderationUndoSchema } from '../src/api/community.ts';
import { crashStatusUndoSchema } from '../src/api/crash.ts';
import { feedbackUndoSchema } from '../src/api/feedback.ts';
import { banUndoSchema } from '../src/api/users.ts';

const feedbackUndo = () => ({ action: 'undo_feedback_status', id: 'feedback-1', value: { status: 'new', token: '0123456789abcdef'.repeat(2) } });

test('撤销只在点击时提交校验后的服务端快照', async () => {
  const raw = { ...feedbackUndo(), unexpected: '不应发送' };
  const sent = [];
  const undo = createActionUndo(feedbackUndoSchema, raw, action => action.id === 'feedback-1', async action => {
    sent.push(action);
    return { ok: true };
  });
  assert.equal(typeof undo, 'function');
  assert.deepEqual(sent, []);
  raw.id = 'another-feedback';
  raw.value.status = 'resolved';
  assert.equal(await undo(), undefined);
  assert.deepEqual(sent, [feedbackUndo()]);
});

test('无效撤销信息或目标不匹配时不创建回调', () => {
  const run = async () => { assert.fail('不应执行不适用的撤销'); };
  for (const raw of [undefined, null, {}, { ...feedbackUndo(), action: 'resolve_feedback' }, { ...feedbackUndo(), value: { status: 'new', token: 'bad' } }]) {
    assert.equal(createActionUndo(feedbackUndoSchema, raw, () => { assert.fail('不应检查未通过校验的目标'); }, run), undefined);
  }
  assert.equal(createActionUndo(feedbackUndoSchema, feedbackUndo(), action => action.id === 'another-feedback', run), undefined);
  assert.equal(createActionUndo(feedbackUndoSchema, feedbackUndo(), action => action.value.status === 'resolved', run), undefined);
});

test('撤销保留调用方的清理和原始错误', async () => {
  const conflict = new Error('合成冲突');
  let refreshed = false;
  const undo = createActionUndo(feedbackUndoSchema, feedbackUndo(), () => true, async () => {
    try {
      throw conflict;
    } finally {
      refreshed = true;
    }
  });
  await assert.rejects(undo, error => error === conflict);
  assert.equal(refreshed, true);
});

test('五种撤销结构沿用相同的小写十六进制版本边界', () => {
  const cases = [
    [feedbackUndoSchema, feedbackUndo()],
    [crashStatusUndoSchema, { action: 'crash_group_status', id: 'crash-1', value: { status: 'open' } }],
    [banUndoSchema, { action: 'undo_user_ban', id: 'user-1', value: { banned: false } }],
    [categoryUndoSchema, { action: 'set_skin_category', section: 'skins', id: 'skin-1', value: { category: 'nature' } }],
    [moderationUndoSchema, { action: 'undo_moderation', section: 'skins', id: 'skin-1', value: { moderation: 'pending', previous: null, reason: null } }],
  ];
  for (const [schema, raw] of cases) {
    for (const token of ['0'.repeat(32), 'abcdef0123456789'.repeat(2)]) {
      assert.equal(schema.safeParse({ ...raw, value: { ...raw.value, token } }).success, true);
    }
    for (const token of [undefined, null, 123, '', 'a'.repeat(31), 'a'.repeat(33), 'g'.repeat(32), 'A'.repeat(32), ` ${'a'.repeat(32)}`]) {
      assert.equal(schema.safeParse({ ...raw, value: { ...raw.value, token } }).success, false);
    }
  }
});

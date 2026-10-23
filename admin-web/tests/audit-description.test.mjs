import assert from 'node:assert/strict';
import { test } from 'node:test';
import { auditEntrySchema, describeAudit } from '../src/api/perm.ts';

const sections = [
  ['skins', '键盘皮肤', 'delete_skin', '键盘皮肤'],
  ['candidate-skins', '候选窗皮肤', 'delete_candidate_skin', '候选窗皮肤'],
  ['plugins', '插件', 'delete_plugin', '插件'],
  ['dictionaries', '社区词库', 'delete_dictionary', '社区词库'],
  ['replies', '回复模板', 'delete_reply', '回复'],
  ['phrases', '短语包', 'delete_phrase', '短语包'],
];

function describe(action, detail = {}, target = 'synthetic-item') {
  return describeAudit(auditEntrySchema.parse({ id: 1, action, target, detail, created_at: '2026-10-24T00:00:00Z' }));
}

test('六类社区作品的审核、批量审核和撤销均显示具体内容类型', () => {
  for (const [section, label] of sections) {
    assert.equal(describe('approve_content', { section }), `通过了${label}「synthetic-item」`);
    assert.equal(describe('remove_content', { section, reason: '测试原因' }), `下架了${label}「synthetic-item」（测试原因）`);
    assert.equal(describe('restore_content', { section }), `恢复了${label}「synthetic-item」`);
    assert.equal(describe('undo_moderation', { section, name: '测试作品' }), `撤销了${label}「测试作品」的审核操作`);
    assert.equal(describe('approve_content', { section, count: 6 }, ''), `通过了${label} 6 项`);
  }
});

test('六类社区删除动作均保留目标和原因', () => {
  for (const [, , action, label] of sections) {
    assert.equal(describe(action, { reason: '测试原因' }), `删除了${label}「synthetic-item」（测试原因）`);
  }
});

test('反馈处理和重新打开使用中文描述并保留目标和原因', () => {
  assert.equal(describe('resolve_feedback', { reason: '测试原因' }), '将反馈「synthetic-item」标记为已处理（测试原因）');
  assert.equal(describe('reopen_feedback', { reason: '测试原因' }), '重新打开了反馈「synthetic-item」（测试原因）');
});

test('未知动作和分区仍保留兼容显示', () => {
  assert.equal(describe('future_action', { reason: '测试原因' }), 'future_action synthetic-item（测试原因）');
  assert.equal(describe('approve_content', { section: 'future' }), '通过了社区内容「synthetic-item」');
});

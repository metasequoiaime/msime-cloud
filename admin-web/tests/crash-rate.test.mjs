import assert from 'node:assert/strict';
import { test } from 'node:test';
import { deviceRateSub, excludedVersionsNote, INSUFFICIENT, sessionRateText } from '../src/utils/crash-rate.ts';

test('有比率时显示百分比，即使部分版本数据不足', () => {
  assert.equal(sessionRateText(0.5, 3, 2), '50.00%');
  assert.equal(sessionRateText(1, 1, 0), '100.00%');
  assert.equal(sessionRateText(0, 1, 0), '0.00%');
});

test('所有版本都数据不足时显示会话数据不足，没有会话时显示破折号', () => {
  assert.equal(sessionRateText(null, 1, 1), INSUFFICIENT);
  assert.equal(sessionRateText(null, 5, 5), '会话数据不足');
  assert.equal(sessionRateText(null, 0, 0), '—');
  assert.equal(sessionRateText(undefined, 0, 0), '—');
  // 有数据充分的版本却没有比率不应出现，按无数据显示而不是误报数据不足。
  assert.equal(sessionRateText(null, 2, 1), '—');
});

test('排除说明只在有版本被排除时出现', () => {
  assert.equal(excludedVersionsNote(0), '');
  assert.equal(excludedVersionsNote(2), '2 个版本会话数据不足，未计入');
  assert.equal(excludedVersionsNote(1200), '1,200 个版本会话数据不足，未计入');
});

test('崩溃设备说明显示崩溃设备与活跃设备数', () => {
  assert.equal(deviceRateSub(72, 2050), '72 / 2,050 台');
  assert.equal(deviceRateSub(0, 0), '0 / 0 台');
});

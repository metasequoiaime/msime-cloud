import assert from 'node:assert/strict';
import { test } from 'node:test';
import { formatDateTime, formatLocaleDateTime, relativeTime } from '../src/utils/time.ts';

test('日期和日志时间按本地时间显示，支持各页面使用的格式', () => {
  const value = '2026-10-14T09:05:07.123';
  assert.equal(formatDateTime(value), '2026-10-14 09:05');
  assert.equal(formatDateTime(value, 'yyyy-MM-dd'), '2026-10-14');
  assert.equal(formatDateTime(value, 'HH:mm:ss.SSS'), '09:05:07.123');
});

test('带时区的日期转换为浏览器本地时间，跨日仍显示正确日期', () => {
  const date = new Date(2026, 9, 14, 0, 5);
  assert.equal(formatDateTime(date.toISOString()), '2026-10-14 00:05');
});

test('无效日期和空字符串保留原文，避免显示记录时抛出异常', () => {
  for (const value of ['无效日期', '2026-99-99', '']) {
    assert.equal(formatDateTime(value), value);
    assert.equal(relativeTime(value), value);
  }
});

test('相对时间使用中文，并区分过去和未来', context => {
  context.mock.method(Date, 'now', () => new Date('2026-10-14T09:00:00Z').getTime());
  assert.equal(relativeTime('2026-10-14T08:00:00Z'), '大约 1 小时前');
  assert.equal(relativeTime('2026-10-14T10:00:00Z'), '大约 1 小时内');
  assert.equal(relativeTime('2026-10-14T09:00:00Z'), '不到 1 分钟前');
});

test('中文本地日期保留年月日顺序、秒数和本地时区跨日', () => {
  const date = new Date(2026, 0, 1, 0, 5, 7);
  const digits = formatLocaleDateTime(date.toISOString()).match(/\d+/g).map(Number);
  assert.deepEqual(digits, [2026, 1, 1, 0, 5, 7]);
});

test('中文本地日期保留页面既有的无效日期文案', () => {
  for (const value of ['', '无效日期', '2026-99-99']) {
    assert.equal(formatLocaleDateTime(value), 'Invalid Date');
  }
});

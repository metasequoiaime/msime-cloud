import assert from 'node:assert/strict';
import { test } from 'node:test';
import { platformLabel, platformOptions } from '../src/api/downloads.ts';
import { noticePlatforms } from '../src/api/notice.ts';

test('下载筛选和公告目标使用相同的平台顺序与名称', () => {
  assert.deepEqual(platformOptions, noticePlatforms);
  assert.equal(platformOptions.length, 6);
});

test('下载平台标签保留别名、空值和未知平台的显示规则', () => {
  for (const [input, label] of [
    [' WIN64 ', 'Windows'],
    [' DARWIN ', 'macOS'],
    ['linux', 'Linux'],
    ['ANDROID', 'Android'],
    ['iPadOS', 'iOS'],
    ['OpenHarmony', 'HarmonyOS'],
    ['', '未知平台'],
    ['  ', '  '],
    [' Custom-Platform ', ' Custom-Platform '],
    ['constructor', 'constructor'],
    ['__proto__', '__proto__'],
  ]) {
    assert.equal(platformLabel(input), label);
  }
});

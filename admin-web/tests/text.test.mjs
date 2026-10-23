import assert from 'node:assert/strict';
import { test } from 'node:test';
import { firstLine, initial } from '../src/utils/text.ts';

test('取第一行并去掉首尾空白', () => {
  for (const [input, expected] of [
    ['  第一行  \n第二行', '第一行'],
    ['第一行\n第二行\n第三行', '第一行'],
    ['\n第二行', ''],
    ['\r\n第二行', ''],
    ['  单行文本  ', '单行文本'],
    ['', ''],
  ]) {
    assert.equal(firstLine(input), expected);
  }
});

test('头像首字母按 Unicode 字符取首字并统一大写', () => {
  for (const [input, expected] of [
    ['  alice', 'A'],
    ['水杉管理员', '水'],
    ['équipe', 'É'],
    ['', '?'],
    ['   ', '?'],
  ]) {
    assert.equal(initial(input), expected);
  }
});

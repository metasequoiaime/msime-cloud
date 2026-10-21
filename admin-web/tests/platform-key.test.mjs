import assert from 'node:assert/strict';
import { test } from 'node:test';
import { platformKey, platformName } from '../src/utils/platform.ts';

test('平台键共用下载页的全部别名规范化', () => {
  for (const [expected, aliases] of [
    ['windows', ['windows', 'win', 'win32', 'win64']],
    ['macos', ['macos', 'mac', 'osx', 'darwin']],
    ['harmony', ['harmony', 'harmonyos', 'ohos', 'openharmony']],
    ['ios', ['ios', 'iphone', 'ipados']],
    ['linux', ['linux']],
    ['android', ['android']],
  ]) {
    for (const alias of aliases) {
      assert.equal(platformKey(` ${alias.toUpperCase()} `), expected);
    }
  }
});

test('未知平台规范键保留小写，显示名称保留原文', () => {
  for (const [input, key] of [
    ['', ''],
    ['  ', ''],
    [' Custom-Platform ', 'custom-platform'],
    ['constructor', 'constructor'],
    [' CONSTRUCTOR ', 'constructor'],
    ['__proto__', '__proto__'],
  ]) {
    assert.equal(platformKey(input), key);
    assert.equal(platformName(input), input);
  }
});

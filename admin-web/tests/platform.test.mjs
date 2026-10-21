import assert from 'node:assert/strict';
import { test } from 'node:test';
import { platformName, platformOptions } from '../src/utils/platform.ts';

test('平台选项保持后台约定的显示顺序和名称', () => {
  assert.deepEqual(platformOptions, [
    { key: 'windows', label: 'Windows' },
    { key: 'macos', label: 'macOS' },
    { key: 'linux', label: 'Linux' },
    { key: 'android', label: 'Android' },
    { key: 'ios', label: 'iOS' },
    { key: 'harmony', label: 'HarmonyOS' },
  ]);
  for (const option of platformOptions) {
    assert.equal(platformName(option.key), option.label);
  }
});

test('平台名称共用大小写不敏感的别名与未知值回退', () => {
  assert.equal(platformName('WINDOWS'), 'Windows');
  assert.equal(platformName('harmonyos'), 'HarmonyOS');
  assert.equal(platformName('OHOS'), 'HarmonyOS');
  assert.equal(platformName('custom-platform'), 'custom-platform');
});

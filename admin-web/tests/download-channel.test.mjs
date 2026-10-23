import assert from 'node:assert/strict';
import { test } from 'node:test';
import { channelLabel } from '../src/api/downloads.ts';

test('遥测下载渠道使用已知中文名，缺省渠道显示未上报', () => {
  for (const [channel, label] of [
    ['cn-mirror', '官网镜像（国内）'],
    ['website', '官网'],
    ['github', 'GitHub（客户端上报）'],
    ['app-store', 'App Store'],
    ['testflight', 'TestFlight'],
    ['appgallery', 'AppGallery'],
    ['google-play', 'Google Play'],
    [null, '未上报渠道'],
    ['', '未上报渠道'],
  ]) {
    assert.equal(channelLabel({ source: 'telemetry', channel }), label);
  }
});

test('未知下载渠道保留原文，原型属性名不能变成对象或函数', () => {
  for (const channel of ['custom-channel', 'constructor', '__proto__', 'toString', 'hasOwnProperty']) {
    assert.equal(channelLabel({ source: 'telemetry', channel }), channel);
  }
});

test('GitHub 快照按来源显示渠道，不受遥测渠道名影响', () => {
  for (const channel of ['github', 'constructor', '__proto__', null]) {
    assert.equal(channelLabel({ source: 'github_release', channel }), 'GitHub Release');
  }
});

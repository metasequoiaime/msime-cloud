import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { test } from 'node:test';
import { candidateColor, candidateImagePath, candidatePalette, parseCandidateDesign } from '../src/candidate-design.ts';

const shared = JSON.parse(readFileSync(new URL('../../internal/skins/testdata/client_dialect.json', import.meta.url), 'utf8'));

test('客户端接受的共享清单都能进入候选窗预览', () => {
  for (const entry of shared.cases.filter(entry => entry.reason === null)) {
    const template = shared.templates[entry.template];
    let manifest = entry.manifest ?? template?.manifest;
    if (!manifest) continue;
    manifest = manifest.replaceAll('{id}', entry.id ?? template.id);
    for (const [from, to] of entry.replace ?? []) manifest = manifest.replace(from, to);
    manifest = (entry.prepend ?? '') + manifest + (entry.append ?? '');
    if (entry.pad_to) manifest += '#' + 'x'.repeat(Math.max(0, entry.pad_to - manifest.length - 1));
    assert.ok(parseCandidateDesign(manifest), entry.name);
  }
});

test('候选窗颜色遵循客户端格式，非法值保留基础配色', () => {
  assert.equal(candidateColor('#abc'), '#aabbcc');
  assert.equal(candidateColor('rgba(90, 106, 150, .5)'), '#5a6a9680');
  assert.equal(candidateColor('transparent'), '#00000000');
  for (const color of ['var(--bad)', 'url(https://example.test/a)', 'rgba(256,0,0,1)', '#gggggg', 'rgb(1,2,-1)']) assert.equal(candidateColor(color), null);
  const design = parseCandidateDesign(shared.templates.styled.manifest.replaceAll('{id}', 'sample'));
  assert.ok(design);
  design.base = 'wechat';
  design.candidate = { light: { surface: 'not a colour' } };
  const { colors, radius } = candidatePalette(design, 'light');
  assert.equal(colors.surface, '#F7F7F7');
  assert.equal(colors.selected_text, '#FFFFFF');
  assert.equal(radius, 12);
});

test('候选窗图片只能读取包内 PNG/JPEG，禁止外部和越界路径', () => {
  assert.equal(candidateImagePath('assets/装饰.png'), 'assets/装饰.png');
  assert.equal(candidateImagePath('background.JPG'), 'background.JPG');
  for (const path of [undefined, '', '../a.png', 'assets/../a.png', '/a.png', 'a\\b.png', 'https://example.test/a.png', 'a.png?q=1', 'a.png#x', 'a.svg', 'a\u0000.png']) assert.equal(candidateImagePath(path), null);
});

import assert from 'node:assert/strict';
import { test } from 'node:test';
import { createListeners } from '../src/utils/listeners.ts';

test('订阅不会立即通知，发送通知时同步按注册顺序调用', () => {
  const listeners = createListeners();
  const calls = [];
  listeners.subscribe(() => calls.push('first'));
  listeners.subscribe(() => calls.push('second'));
  assert.deepEqual(calls, []);
  listeners.emit();
  assert.deepEqual(calls, ['first', 'second']);
});

test('取消订阅后不再通知，重复清理不会移除其他订阅', () => {
  const { subscribe, emit } = createListeners();
  const calls = [];
  const unsubscribe = subscribe(() => calls.push('first'));
  subscribe(() => calls.push('second'));
  unsubscribe();
  unsubscribe();
  emit();
  assert.deepEqual(calls, ['second']);
});

test('不同实例的通知和取消订阅互不影响', () => {
  const appearance = createListeners();
  const overlay = createListeners();
  const calls = [];
  const unsubscribe = appearance.subscribe(() => calls.push('appearance'));
  overlay.subscribe(() => calls.push('overlay'));
  appearance.emit();
  assert.deepEqual(calls, ['appearance']);
  unsubscribe();
  overlay.emit();
  appearance.emit();
  assert.deepEqual(calls, ['appearance', 'overlay']);
});

test('通知期间取消尚未调用的订阅后，该订阅不再执行', () => {
  const listeners = createListeners();
  const calls = [];
  let unsubscribe = () => {};
  listeners.subscribe(() => { calls.push('first'); unsubscribe(); });
  unsubscribe = listeners.subscribe(() => calls.push('second'));
  listeners.emit();
  assert.deepEqual(calls, ['first']);
});

import assert from 'node:assert/strict';
import { test } from 'node:test';
import { applyCells, canEditCell, cellAddress, clipboardText, draftError, emptyHistory, makeSheetRows, parseClipboard, redoHistory, undoHistory, updateHistory } from '../src/pages/dictionary/sheet-model.ts';

const path = 'packs/programming/quanpin.txt';
const defaults = ['', '', '100'];
const originals = [{ line: 17, columns: ['示例', "shi'li", '100'] }];
const row = makeSheetRows(originals, [], defaults, true)[0];

test('恢复原值只撤销对应字段，整块修改保持原子性', () => {
  const first = applyCells(path, [], [{ row, col: 0, value: '新词' }, { row, col: 2, value: '200' }]);
  const edited = makeSheetRows(originals, first, defaults, true)[0];
  const next = applyCells(path, first, [{ row: edited, col: 0, value: '示例' }, { row: edited, col: 1, value: 'new' }]);
  assert.deepEqual(next[0].columns, ['示例', 'new', '200']);
  assert.equal(next[0].line, 17);
  assert.deepEqual(applyCells(path, next, originals[0].columns.map((value, col) => ({ row: edited, col, value }))), []);
  assert.throws(() => applyCells(path, first, [{ row: edited, col: 0, value: '另一个' }, { row: edited, col: 1, value: '非法\n字段' }]));
  assert.deepEqual(first[0].columns, ['新词', "shi'li", '200']);
});

test('自定义已有词条只读，译文追加且保留原始行号', () => {
  assert.equal(canEditCell('custom/words.txt', row, 0), false);
  assert.equal(canEditCell('sources/pinyin/words.txt', row, 0), false);
  assert.throws(() => applyCells('sources/pinyin/words.txt', [], [{ row, col: 0, value: '新词' }]));
  assert.throws(() => applyCells('custom/words.txt', [], [{ row, col: 0, value: '新词' }]));
  const translationOriginals = [{ line: 19, columns: ['示例', 'old'] }];
  const translation = makeSheetRows(translationOriginals, [], ['', ''], true)[0];
  const next = applyCells('custom/translations.txt', [], [{ row: translation, col: 1, value: 'new' }]);
  assert.equal(next[0].operation, 'add');
  assert.equal(next[0].line, 0);
  assert.equal(next[0].sourceLine, 19);
  assert.deepEqual(makeSheetRows(translationOriginals, next, ['', ''], true)[0].columns, ['示例', 'new']);
  assert.throws(() => applyCells('custom/translations.txt', next, [{ row: translation, col: 0, value: '替换原词' }]));
});

test('批量新增各行独立，100 项限制不留下部分修改', () => {
  const rows = Array.from({ length: 100 }, (_, index) => ({ id: `blank-${index}`, line: 0, columns: defaults, blank: true }));
  const next = applyCells(path, [], rows.flatMap((row, index) => [`词${index}`, `code${index}`, '100'].map((value, col) => ({ row, col, value }))));
  assert.equal(next.length, 100);
  assert.equal(new Set(next.map(draft => draft.id)).size, 100);
  assert.equal(makeSheetRows(originals, next, defaults, true).length, 101);
  assert.throws(() => applyCells(path, next, [{ row: { ...rows[0], id: 'overflow' }, col: 0, value: '更多' }]));
  assert.equal(next.length, 100);
});

test('编辑新增行不改变行顺序，连续录入仍指向同一词条', () => {
  const rows = ['词一', '词二'].map((word, index) => ({ id: `new-${index}`, line: 0, columns: [word, 'code', '100'] }));
  const drafts = rows.map(row => ({ id: row.id, operation: 'add', line: 0, columns: row.columns }));
  const next = applyCells(path, drafts, [{ row: rows[0], col: 2, value: '200' }]);
  assert.deepEqual(next.map(draft => draft.id), ['new-0', 'new-1']);
  assert.deepEqual(makeSheetRows([], next, defaults, true).slice(0, 2).map(row => row.columns), [['词一', 'code', '200'], ['词二', 'code', '100']]);
});

test('不完整行可暂存，保存校验权重及字段', () => {
  const blank = makeSheetRows([], [], defaults, true)[0];
  const next = applyCells(path, [], [{ row: blank, col: 0, value: '未完成' }]);
  assert.equal(draftError(path, next[0]), '请填写全部字段');
  for (const weight of ['0', '10001', '1.5', '-1']) assert.ok(draftError(path, { ...next[0], columns: ['词', 'code', weight] }));
  assert.equal(draftError(path, { ...next[0], columns: ['词', 'code', '10000'] }), undefined);
  assert.ok(draftError('packs/programming/english.txt', { ...next[0], columns: ['code', 'Code', '11'] }));
  assert.throws(() => applyCells(path, [], [{ row: blank, col: 0, value: '词'.repeat(334) }]));
});

test('批量操作整体撤销重做，新修改清除重做历史', () => {
  const next = applyCells(path, [], [{ row, col: 0, value: '新词' }, { row, col: 2, value: '300' }]);
  const history = updateHistory(emptyHistory, next);
  assert.deepEqual(undoHistory(history).present, []);
  assert.deepEqual(redoHistory(undoHistory(history)).present, next);
  assert.equal(updateHistory(history, next), history);
  assert.deepEqual(updateHistory(undoHistory(history), [{ ...next[0], columns: ['其他', 'code', '100'] }]).future, []);
});

test('Excel TSV 双引号、CRLF 和多列复制可往返', () => {
  const matrix = [['示例', 'quote " code', '100'], ['词二', 'code', '200']];
  assert.deepEqual(parseClipboard(clipboardText(matrix) + '\r\n'), matrix);
  assert.deepEqual(parseClipboard('plain\tcode\t100\r\nnext\tcode\t200'), [['plain', 'code', '100'], ['next', 'code', '200']]);
  assert.deepEqual(parseClipboard('"多\n行"\tcode'), [['多\n行', 'code']]);
  assert.throws(() => parseClipboard('"未闭合'));
  assert.throws(() => parseClipboard('"闭合"非法'));
  assert.equal(cellAddress({ row: 99, col: 26 }), 'AA100');
});

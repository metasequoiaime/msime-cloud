import type { DictionaryEdit, DictionaryRow } from "../../api/dictionary";

export type Draft = DictionaryEdit & { id: string; sourceLine?: number };
export type SheetRow = { id: string; line: number; columns: string[]; original?: DictionaryRow; deleted?: boolean; blank?: boolean };
export type Cell = { row: number; col: number };
export type CellChange = { row: SheetRow; col: number; value: string };
export type History = { past: Draft[][]; present: Draft[]; future: Draft[][] };
export const emptyHistory: History = { past: [], present: [], future: [] };

export function updateHistory(history: History, next: Draft[]): History {
  if (JSON.stringify(history.present) === JSON.stringify(next)) return history;
  return { past: [...history.past.slice(-99), history.present], present: next, future: [] };
}
export function undoHistory(history: History): History {
  const previous = history.past.at(-1);
  return previous ? { past: history.past.slice(0, -1), present: previous, future: [history.present, ...history.future] } : history;
}
export function redoHistory(history: History): History {
  const next = history.future[0];
  return next ? { past: [...history.past, history.present], present: next, future: history.future.slice(1) } : history;
}

export function makeSheetRows(originals: DictionaryRow[], drafts: Draft[], defaults: string[], append: boolean): SheetRow[] {
  const rows: SheetRow[] = originals.map(original => {
    const draft = drafts.find(entry => (entry.sourceLine ?? entry.line) === original.line);
    return { id: `line-${original.line}`, line: original.line, columns: draft?.operation === "delete" ? original.columns : draft?.columns ?? original.columns, original, deleted: draft?.operation === "delete" };
  });
  rows.push(...drafts.filter(entry => entry.operation === "add" && !entry.sourceLine).map(entry => ({ id: entry.id, line: 0, columns: entry.columns })));
  if (append && drafts.length < 100) rows.push({ id: "blank", line: 0, columns: defaults, blank: true });
  return rows;
}

export function canEditCell(path: string, row: SheetRow, col: number): boolean {
  if (row.deleted || !/^(?:custom\/(?:words|english|translations)|packs\/[^/]+\/(?:quanpin|english|translations))\.txt$/.test(path)) return false;
  return !row.original || !path.startsWith("custom/") || (path.endsWith("translations.txt") && col === 1);
}

// 一次粘贴或填充作为一个变更处理；任何目标只读时整批拒绝，不留下部分修改。
export function applyCells(path: string, drafts: Draft[], changes: CellChange[]): Draft[] {
  const groups = new Map<string, { row: SheetRow; columns: string[] }>();
  for (const { row, col, value } of changes) {
    if (!canEditCell(path, row, col)) throw new Error("选区包含只读单元格，请选择可编辑的词条或新增行。");
    if (/[\t\r\n]/.test(value) || new TextEncoder().encode(value).length > 1000) throw new Error("单元格最多 1000 字节，不能包含换行或制表符。");
    const group = groups.get(row.id) ?? { row, columns: [...row.columns] };
    group.columns[col] = value.trim();
    groups.set(row.id, group);
  }
  let next = [...drafts];
  for (const { row, columns } of groups.values()) {
    const id = row.blank ? crypto.randomUUID() : row.id;
    const position = next.findIndex(entry => entry.id === id);
    next = next.filter(entry => entry.id !== id);
    if (row.original && columns.every((field, index) => field === row.original?.columns[index])) continue;
    if (!row.original && columns.every(field => !field)) continue;
    const append = !row.original || path.startsWith("custom/");
    next.splice(position < 0 ? next.length : position, 0, { id, operation: append ? "add" : "replace", line: append ? 0 : row.line, sourceLine: row.original && append ? row.line : undefined, columns });
  }
  if (next.length > 100) throw new Error("一次最多保存 100 项变更，请先保存当前变更。");
  return next;
}

export function draftError(path: string, draft: Draft): string | undefined {
  if (draft.operation === "delete") return;
  const translation = path.endsWith("translations.txt");
  if (draft.columns.length !== (translation ? 2 : 3) || draft.columns.some(value => !value.trim() || /[\t\r\n]/.test(value))) return "请填写全部字段";
  if (!translation) {
    return cellError(path, draft, 2);
  }
}

export function cellError(path: string, draft: Draft, col: number): string | undefined {
  if (draft.operation === "delete") return;
  const value = draft.columns[col] ?? "";
  if (!value.trim()) return "此字段尚未填写";
  if (/[\t\r\n]/.test(value) || new TextEncoder().encode(value).length > 1000) return "字段不能包含换行或制表符，最多 1000 字节";
  if (col === 2 && !path.endsWith("translations.txt")) {
    const maximum = path.startsWith("packs/") && path.endsWith("english.txt") ? 10 : 10000;
    if (!/^\d+$/.test(value) || Number(value) < 1 || Number(value) > maximum) return `权重应为 1–${maximum} 的整数`;
  }
}

// 单个单元格向外粘贴；选区大于数据时按完整矩形重复填充，避免只覆盖一部分。
export function fitClipboard(matrix: string[][], height: number, width: number): string[][] {
  const columns = Math.max(0, ...matrix.map(row => row.length));
  if (!matrix.length || !columns) throw new Error("剪贴板没有可粘贴的词条。");
  const rectangle = matrix.map(row => Array.from({ length: columns }, (_, col) => row[col] ?? ""));
  if (height === 1 && width === 1) return rectangle;
  if (height <= matrix.length && width <= columns) return rectangle;
  if (height % matrix.length || width % columns) throw new Error("粘贴内容与选区大小不匹配，请选择一个起始单元格，或选择行列数为粘贴内容整数倍的区域。");
  return Array.from({ length: height }, (_, row) => Array.from({ length: width }, (_, col) => rectangle[row % matrix.length][col % columns]));
}

export function cellAddress(cell: Cell): string {
  let column = "", n = cell.col + 1;
  while (n > 0) { n--; column = String.fromCharCode(65 + n % 26) + column; n = Math.floor(n / 26); }
  return `${column}${cell.row + 1}`;
}

// 支持 Excel 的制表符分列、双引号转义和 CRLF；带换行的字段交由单元格校验拒绝。
export function parseClipboard(text: string): string[][] {
  const rows: string[][] = [];
  let row: string[] = [], field = "", quoted = false, closed = false;
  const source = text.replace(/\r\n?/g, "\n");
  for (let i = 0; i < source.length; i++) {
    const char = source[i];
    if (quoted) {
      if (char === '"' && source[i + 1] === '"') { field += '"'; i++; }
      else if (char === '"') { quoted = false; closed = true; }
      else field += char;
    } else if (char === "\t" || char === "\n") {
      row.push(field); field = ""; closed = false;
      if (char === "\n") { rows.push(row); row = []; }
    } else if (char === '"' && field === "" && !closed) quoted = true;
    else { if (closed) throw new Error("粘贴内容的引号格式无效。"); field += char; }
  }
  if (quoted) throw new Error("粘贴内容的引号未闭合。");
  row.push(field); rows.push(row);
  if (source.endsWith("\n")) rows.pop();
  return rows;
}

export function clipboardText(rows: string[][]): string {
  return rows.map(row => row.map(value => /[\t\n\r"]/.test(value) ? `"${value.replaceAll('"', '""')}"` : value).join("\t")).join("\r\n");
}

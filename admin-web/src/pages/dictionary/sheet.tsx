import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import type { ClipboardEvent, KeyboardEvent, PointerEvent } from "react";
import { ArrowDownToLine, CircleAlert, Copy, Plus, Redo2, Trash2, Undo2 } from "lucide-react";
import type { DictionaryRow } from "../../api/dictionary";
import { Button } from "../../ui/button";
import { cn } from "../../ui/cn";
import { applyCells, canEditCell, cellAddress, cellError, clipboardText, draftError, fitClipboard, makeSheetRows, parseClipboard } from "./sheet-model";
import type { Cell, CellChange, Draft, SheetRow } from "./sheet-model";

type Editor = { cell: Cell; value: string };
type Props = {
  path: string; originals: DictionaryRow[]; labels: string[]; defaults: string[]; drafts: Draft[];
  editable: boolean; locked: boolean; hasMore: boolean; focused: boolean; canUndo: boolean; canRedo: boolean;
  onChange: (drafts: Draft[]) => void; onEditing: (editing: boolean) => void;
  onUndo: () => void; onRedo: () => void; onSave: (drafts: Draft[]) => void;
};

export function DictionarySheet({ path, originals, labels, defaults, drafts, editable, locked, hasMore, focused: expanded, canUndo, canRedo, onChange, onEditing, onUndo, onRedo, onSave }: Props) {
  const rows = useMemo(() => makeSheetRows(originals, drafts, defaults, editable), [originals, drafts, defaults, editable]);
  const [active, setActive] = useState<Cell>({ row: 0, col: 0 });
  const [anchor, setAnchor] = useState<Cell>(active);
  const [editor, setEditor] = useState<Editor>();
  const editorRef = useRef<Editor | undefined>(undefined);
  const input = useRef<HTMLInputElement>(null);
  const dragging = useRef(false);
  const focusWanted = useRef(false);
  const caretEnd = useRef(false);
  const tabOrigin = useRef<number | undefined>(undefined);
  const [, setFocusVersion] = useState(0);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const draftMap = useMemo(() => new Map(drafts.map(draft => [draft.id, draft])), [drafts]);
  const problems = rows.flatMap((row, index) => {
    const draft = draftMap.get(row.id);
    return draft && draftError(path, draft) ? [{ row: index, col: Math.max(0, labels.findIndex((_, col) => cellError(path, draft, col))) }] : [];
  });
  const top = Math.min(active.row, anchor.row), bottom = Math.max(active.row, anchor.row);
  const left = Math.min(active.col, anchor.col), right = Math.max(active.col, anchor.col);
  const count = (bottom - top + 1) * (right - left + 1);
  const address = cellAddress(active);
  const activeRow = rows[active.row];
  const writable = editable && !locked && Boolean(activeRow && canEditCell(path, activeRow, active.col));
  const currentValue = editor?.value ?? activeRow?.columns[active.col] ?? "";
  const refocus = useCallback(() => { focusWanted.current = true; setFocusVersion(value => value + 1); }, []);
  useLayoutEffect(() => {
    if (!focusWanted.current || dragging.current || !input.current) return;
    input.current.focus({ preventScroll: true });
    if (caretEnd.current) input.current.setSelectionRange(input.current.value.length, input.current.value.length);
    else input.current.select();
    input.current.closest("td")?.scrollIntoView({ block: "nearest", inline: "nearest" });
    caretEnd.current = false; focusWanted.current = false;
  });
  useEffect(() => {
    const stop = () => { if (dragging.current) { dragging.current = false; refocus(); } };
    window.addEventListener("pointerup", stop);
    return () => { window.removeEventListener("pointerup", stop); onEditing(false); };
  }, [onEditing, refocus]);
  useEffect(() => {
    if (!rows.length) return;
    if (active.row >= rows.length) { setActive({ row: rows.length - 1, col: active.col }); refocus(); }
    if (anchor.row >= rows.length) setAnchor({ row: rows.length - 1, col: anchor.col });
  }, [active.row, active.col, anchor.row, anchor.col, rows.length, refocus]);
  const clearEditor = () => { editorRef.current = undefined; setEditor(undefined); onEditing(false); };
  const patch = (changes: CellChange[], base = drafts) => {
    if (!editable || locked) return null;
    try { const next = applyCells(path, base, changes); onChange(next); setError(""); return next; }
    catch (reason) { setError(reason instanceof Error ? reason.message : "无法修改所选单元格。"); return null; }
  };
  const commit = () => {
    const entry = editorRef.current;
    if (!entry) return drafts;
    const row = rows[entry.cell.row];
    if (!row || locked) return null;
    const next = entry.value === (row.columns[entry.cell.col] ?? "") ? drafts : patch([{ row, col: entry.cell.col, value: entry.value }]);
    if (next) clearEditor();
    return next;
  };
  const begin = (value = currentValue, end = true) => {
    if (!writable) { setError("此单元格只读，可选中复制；新增词条请在表格末尾录入。"); return; }
    const entry = { cell: active, value }; editorRef.current = entry; setEditor(entry); onEditing(true); setError(""); setNotice(""); setAnchor(active);
    caretEnd.current = end; refocus();
  };
  const changeValue = (value: string) => {
    const entry = { cell: active, value }; editorRef.current = entry; setEditor(entry); onEditing(true); setError(""); setNotice("");
  };
  const select = (cell: Cell, extend = false, keepTabOrigin = false) => {
    if (!keepTabOrigin) tabOrigin.current = undefined;
    setNotice(""); setActive(cell); if (!extend) setAnchor(cell); refocus();
  };
  const move = (rowDelta: number, colDelta: number, extend = false, tab = false) => {
    const wasEditing = Boolean(editorRef.current);
    const next = commit(); if (!next) return;
    const view = makeSheetRows(originals, next, defaults, editable);
    if (!view.length) return;
    let row = active.row + rowDelta, col = active.col + colDelta;
    if (tab) {
      tabOrigin.current ??= active.col;
      const step = colDelta > 0 ? 1 : -1;
      let position = active.row * labels.length + active.col + step;
      while (position >= 0 && position < view.length * labels.length) {
        row = Math.floor(position / labels.length); col = position % labels.length;
        if (!wasEditing || canEditCell(path, view[row], col)) break;
        position += step;
      }
    }
    select({ row: Math.max(0, Math.min(view.length - 1, row)), col: Math.max(0, Math.min(labels.length - 1, col)) }, extend, tab);
  };
  const selectedCells = (view = rows) => {
    const cells: { row: SheetRow; col: number }[] = [];
    for (let r = top; r <= bottom; r++) for (let c = left; c <= right; c++) if (view[r]) cells.push({ row: view[r], col: c });
    return cells;
  };
  const copyText = () => clipboardText(rows.slice(top, bottom + 1).map(row => row.columns.slice(left, right + 1)));
  const copy = (event: ClipboardEvent<HTMLInputElement>) => {
    if (editorRef.current) return;
    event.preventDefault(); event.clipboardData.setData("text/plain", copyText()); setNotice(`已复制 ${count} 个单元格`);
  };
  const copySelection = async () => {
    try { await navigator.clipboard.writeText(copyText()); setNotice(`已复制 ${count} 个单元格`); }
    catch { setError("请使用 Ctrl / ⌘ C 复制选区。"); }
  };
  const paste = (event: ClipboardEvent<HTMLInputElement>) => {
    const text = event.clipboardData.getData("text/plain");
    if (editorRef.current && !/[\t\r\n]/.test(text)) return;
    event.preventDefault();
    if (!editable || locked) { setError("此文件只读，无法粘贴词条。"); return; }
    try {
      const matrix = fitClipboard(parseClipboard(text), bottom - top + 1, right - left + 1);
      const width = Math.max(...matrix.map(row => row.length));
      if (left + width > labels.length) throw new Error(`粘贴超出词库的 ${labels.length} 列，请检查起始单元格。`);
      if (matrix.length > 100) throw new Error("一次最多粘贴 100 行，请分批保存。");
      if (hasMore && top < originals.length && top + matrix.length > originals.length) throw new Error("粘贴区域跨过本页词条。请分批覆盖，或从表格末尾的新增行粘贴新词条。");
      const view = [...rows];
      while (view.length < top + matrix.length) view.push({ id: crypto.randomUUID(), line: 0, columns: defaults, blank: true });
      const changes = matrix.flatMap((row, index) => row.map((value, col) => ({ row: view[top + index], col: left + col, value })));
      const next = patch(changes);
      if (next) { clearEditor(); tabOrigin.current = undefined; setActive({ row: top, col: left }); setAnchor({ row: top + matrix.length - 1, col: left + width - 1 }); setNotice(`已粘贴 ${matrix.length} 行 × ${width} 列 · 可撤销`); refocus(); }
    } catch (reason) { setError(reason instanceof Error ? reason.message : "粘贴格式无效。"); }
  };
  const fillDown = () => {
    if (!editable || locked) return;
    const next = commit(); if (!next) return;
    const view = makeSheetRows(originals, next, defaults, editable);
    const from = top === bottom ? top - 1 : top;
    if (from < 0) return;
    const changes: CellChange[] = [];
    for (let r = top === bottom ? top : top + 1; r <= bottom; r++) for (let c = left; c <= right; c++) changes.push({ row: view[r], col: c, value: view[from].columns[c] ?? "" });
    if (patch(changes, next)) setNotice(`已向下填充 ${changes.length} 个单元格 · 可撤销`); refocus();
  };
  const keyDown = (event: KeyboardEvent<HTMLInputElement>, formula = false) => {
    if (event.nativeEvent.isComposing || event.keyCode === 229) return;
    const mod = event.ctrlKey || event.metaKey;
    if (formula && !["Tab", "Enter", "Escape"].includes(event.key) && !(mod && event.key.toLowerCase() === "s")) return;
    if (mod && event.key.toLowerCase() === "s") { event.preventDefault(); const next = commit(); if (next) onSave(next); return; }
    if (mod && event.key.toLowerCase() === "d") { event.preventDefault(); fillDown(); return; }
    if (mod && !locked && !editorRef.current && ["z", "y"].includes(event.key.toLowerCase())) { event.preventDefault(); if (event.shiftKey || event.key.toLowerCase() === "y") onRedo(); else onUndo(); refocus(); return; }
    if (mod && !editorRef.current && event.key.toLowerCase() === "a") { event.preventDefault(); setAnchor({ row: 0, col: 0 }); setActive({ row: Math.max(0, rows.length - 1 - (rows.at(-1)?.blank ? 1 : 0)), col: labels.length - 1 }); refocus(); return; }
    if (event.key === "Escape") { event.preventDefault(); clearEditor(); setError(""); setAnchor(active); refocus(); return; }
    if (event.key === "Tab") { event.preventDefault(); move(0, event.shiftKey ? -1 : 1, false, true); return; }
    if (event.key === "Enter") { event.preventDefault(); move(event.shiftKey ? -1 : 1, tabOrigin.current === undefined ? 0 : tabOrigin.current - active.col); return; }
    if (!editorRef.current && event.key === "F2") { event.preventDefault(); begin(); return; }
    if (event.key.startsWith("Arrow") && (!editorRef.current || event.key === "ArrowUp" || event.key === "ArrowDown")) {
      event.preventDefault();
      move(event.key === "ArrowUp" ? mod ? -rows.length : -1 : event.key === "ArrowDown" ? mod ? rows.length : 1 : 0, event.key === "ArrowLeft" ? mod ? -labels.length : -1 : event.key === "ArrowRight" ? mod ? labels.length : 1 : 0, event.shiftKey); return;
    }
    if (!editorRef.current && (event.key === "Home" || event.key === "End")) { event.preventDefault(); select({ row: mod ? event.key === "Home" ? 0 : rows.length - 1 : active.row, col: event.key === "Home" ? 0 : labels.length - 1 }, event.shiftKey); return; }
    if (!editorRef.current && (event.key === "Delete" || event.key === "Backspace")) { event.preventDefault(); if (editable && !locked) patch(selectedCells().map(cell => ({ ...cell, value: "" }))); return; }
    if (!editorRef.current && !mod && !event.altKey && event.key.length === 1 && writable) { event.preventDefault(); begin(event.key); }
  };
  const pointerDown = (event: PointerEvent<HTMLTableCellElement>, cell: Cell) => {
    if (event.button !== 0 || (editorRef.current && cell.row === active.row && cell.col === active.col && event.target instanceof HTMLInputElement)) return;
    if (!commit()) return;
    event.preventDefault(); dragging.current = true; select(cell, event.shiftKey);
  };
  const removeRows = () => {
    const next = commit(); if (!next || !editable || locked) return;
    const chosen = rows.slice(top, bottom + 1).filter(row => !row.blank);
    if (chosen.some(row => row.original && path.startsWith("custom/"))) { setError("自定义词库的已有记录只追加，不能删除。"); return; }
    let result = [...next];
    for (const row of chosen) { result = result.filter(entry => entry.id !== row.id); if (row.original) result.push({ id: row.id, operation: "delete", line: row.line, columns: [] }); }
    if (result.length > 100) { setError("一次最多保存 100 项变更。"); return; }
    onChange(result); setError(""); refocus();
  };
  const add = () => { const next = commit(); if (!next) return; const row = makeSheetRows(originals, next, defaults, editable).findIndex(value => value.blank); if (row >= 0) { select({ row, col: 0 }); } };
  const clone = () => {
    const next = commit(); if (!next || !activeRow || !editable || locked || next.length >= 100) return;
    onChange([...next, { id: crypto.randomUUID(), operation: "add", line: 0, columns: [...makeSheetRows(originals, next, defaults, editable)[active.row].columns] }]);
    select({ row: rows.length - 1, col: 0 });
  };
  const selectColumn = (col?: number) => {
    const next = commit(); if (!next) return;
    const view = makeSheetRows(originals, next, defaults, editable).filter(row => !row.blank);
    if (!view.length) return;
    select({ row: 0, col: col ?? 0 }); setAnchor({ row: view.length - 1, col: col ?? labels.length - 1 });
  };
  const locateProblem = () => {
    const next = commit(); if (!next) return;
    const view = makeSheetRows(originals, next, defaults, editable);
    const candidates = view.flatMap((row, index) => {
      const draft = next.find(entry => entry.id === row.id);
      const col = draft ? labels.findIndex((_, col) => cellError(path, draft, col)) : -1;
      return col < 0 ? [] : [{ row: index, col, draft }];
    });
    const problem = candidates.find(cell => cell.row > active.row) ?? candidates[0];
    if (problem?.draft) { select(problem); setNotice(`${cellAddress(problem)} · ${cellError(path, problem.draft, problem.col)}`); }
  };
  return <section aria-label="电子表格编辑器">
    <div className="mb-2 flex flex-wrap items-center gap-1.5">
      <Button size="sm" variant="outline" disabled={!canUndo || locked} onClick={() => { clearEditor(); onUndo(); refocus(); }} title="Ctrl / ⌘ Z"><Undo2 size={14} aria-hidden="true" />撤销</Button>
      <Button size="sm" variant="outline" disabled={!canRedo || locked} onClick={() => { clearEditor(); onRedo(); refocus(); }} title="Ctrl / ⌘ Shift Z"><Redo2 size={14} aria-hidden="true" />重做</Button>
      <Button size="sm" variant="outline" onClick={() => void copySelection()}><Copy size={14} aria-hidden="true" />复制选区</Button>
      {editable && <><Button size="sm" variant="outline" disabled={locked || drafts.length >= 100} onClick={add}><Plus size={14} aria-hidden="true" />新增词条</Button><Button size="sm" variant="outline" disabled={locked || !activeRow || activeRow.blank || activeRow.deleted || drafts.length >= 100} onClick={clone}>复制为新词条</Button><Button size="sm" variant="outline" disabled={locked || (!top && top === bottom)} onClick={fillDown} title="Ctrl / ⌘ D"><ArrowDownToLine size={14} aria-hidden="true" />向下填充</Button><Button size="sm" variant="danger-outline" disabled={locked || !activeRow || activeRow.blank || (path.startsWith("custom/") && rows.slice(top, bottom + 1).some(row => row.original))} onClick={removeRows}><Trash2 size={14} aria-hidden="true" />删除选中行</Button></>}
    </div>
    {problems.length > 0 && <div className="mb-2 flex flex-wrap items-center justify-between gap-2 rounded-lg bg-bad-soft px-3 py-2 text-[12px] text-bad"><span><CircleAlert size={14} className="mr-1 inline" aria-hidden="true" />本页 {problems.length} 行需修正，已保留草稿</span><Button size="sm" variant="ghost" disabled={locked} onClick={locateProblem}>定位问题</Button></div>}
    <div className="mb-2 flex min-w-0 items-center rounded-lg bg-panel-2 ring-1 ring-hair"><span role="status" className="w-20 shrink-0 px-3 py-2 text-center font-mono text-[12px] text-accent-ink" aria-label="当前选区">{count > 1 ? `${cellAddress({ row: top, col: left })}:${cellAddress({ row: bottom, col: right })}` : address}</span><span className="border-l border-hair px-3 text-[12px] text-muted">fx</span><input aria-label="单元格内容" readOnly={!writable} value={currentValue} onChange={event => changeValue(event.target.value)} onBlur={() => { if (!commit()) refocus(); }} onKeyDown={event => keyDown(event, true)} className="h-9 min-w-0 flex-1 bg-transparent pr-3 text-[13px] text-ink outline-none" /></div>
    {error && <p role="alert" className="mb-2 text-[12.5px] text-bad">{error}</p>}
    {/* 保留原生表格结构，单元格通过内部输入框接受键盘操作。 */}
    <div className={cn("overflow-auto rounded-lg ring-1 ring-hair", expanded ? "max-h-[calc(100dvh-320px)] min-h-48" : "max-h-[620px]")}>
      {/* biome-ignore lint/a11y/noNoninteractiveElementToInteractiveRole: 电子表格使用 grid 语义并通过输入框提供键盘交互。 */}
      <table role="grid" aria-label="词条电子表格" aria-readonly={!editable || locked} className="w-full min-w-[520px] table-fixed border-collapse text-left text-[12.5px]">
      <colgroup><col className="w-14" />{labels.map(label => <col key={label} className={label === "权重" ? "w-28" : "w-48"} />)}</colgroup>
      <thead className="sticky top-0 z-20 bg-panel-2 text-muted"><tr><th scope="col" className="sticky left-0 bg-panel-2 text-center font-medium"><button type="button" aria-label="选择全部词条" onClick={() => selectColumn()} className="h-9 w-full px-2 hover:bg-accent-soft">行</button></th>{labels.map((label, col) => <th key={label} scope="col" className="border-l border-hair font-medium"><button type="button" aria-label={`选择${label}列`} onClick={() => selectColumn(col)} className="h-9 w-full px-3 text-left hover:bg-accent-soft"><span className="mr-2 font-mono text-[11px]">{cellAddress({ row: 0, col }).slice(0, -1)}</span>{label}</button></th>)}</tr></thead>
      <tbody>{rows.map((row, r) => {
        const draft = draftMap.get(row.id);
        const invalid = draft && draftError(path, draft);
        return <tr key={row.id}><th scope="row" className={cn("sticky left-0 z-10 border-t border-hair bg-panel-2 text-center font-mono text-[11px] text-muted", invalid && "text-bad")} title={invalid ?? (row.line ? `源文件第 ${row.line} 行` : "新增词条")}><button type="button" tabIndex={-1} onClick={() => { if (!commit()) return; setAnchor({ row: r, col: 0 }); setActive({ row: r, col: labels.length - 1 }); refocus(); }} className="h-9 w-full">{row.blank ? "+" : row.line || "+"}</button></th>
          {labels.map((label, c) => {
            const selected = r >= top && r <= bottom && c >= left && c <= right;
            const focused = r === active.row && c === active.col;
            const value = row.columns[c] ?? "";
            const fieldError = draft ? cellError(path, draft, c) : undefined;
            // 单元格由内部输入框管理焦点，容器不参与 Tab 顺序。
            // biome-ignore lint/a11y/noNoninteractiveElementToInteractiveRole: 原生单元格使用 gridcell 语义，与外层 grid 对应。
            return <td tabIndex={-1} key={label} role="gridcell" aria-selected={selected} title={fieldError ?? value} aria-readonly={!editable || locked || !canEditCell(path, row, c)} data-cell={cellAddress({ row: r, col: c })} onPointerDown={event => pointerDown(event, { row: r, col: c })} onPointerEnter={() => { if (dragging.current) setActive({ row: r, col: c }); }} onDoubleClick={() => { select({ row: r, col: c }); if (editable && !locked && canEditCell(path, row, c)) { const entry = { cell: { row: r, col: c }, value }; editorRef.current = entry; setEditor(entry); onEditing(true); caretEnd.current = true; refocus(); } }} className={cn("relative h-9 border-t border-l border-hair p-0 text-body", selected && "bg-accent-soft", focused && "z-[1] inset-ring-2 inset-ring-accent", draft && !selected && "bg-accent-soft/50", fieldError && "bg-bad-soft/60 inset-ring inset-ring-bad")}>
              {focused ? <input ref={input} aria-label={`${cellAddress({ row: r, col: c })} ${label}`} aria-invalid={Boolean(fieldError)} readOnly={!writable} value={currentValue} onChange={event => changeValue(event.target.value)} onCompositionStart={() => { if (writable && !editorRef.current) { const entry = { cell: active, value }; editorRef.current = entry; setEditor(entry); onEditing(true); } }} onKeyDown={keyDown} onCopy={copy} onPaste={paste} onBlur={() => { if (!commit()) refocus(); }} onFocus={event => { if (!editorRef.current) event.currentTarget.select(); }} className={cn("h-9 w-full min-w-0 bg-transparent px-3 text-[12.5px] text-ink outline-none", row.deleted && "line-through text-muted")} /> : <button type="button" tabIndex={-1} className={cn("block h-9 w-full truncate px-3 text-left", row.deleted && "line-through text-muted", row.blank && "text-muted")} aria-label={`${cellAddress({ row: r, col: c })} ${label}：${value}`} onFocus={() => select({ row: r, col: c })}>{value || (row.blank && c === 0 ? "输入或粘贴新词条…" : "")}</button>}
            </td>;
          })}
        </tr>;
      })}</tbody>
    </table></div>
    <div className="mt-2 flex flex-wrap justify-between gap-2 text-[11.5px] text-muted"><span aria-live="polite">{notice || (editor ? "正在输入" : count > 1 ? `已选中 ${count} 个单元格` : `${address} · ${writable ? "可编辑" : "只读"}`)}</span><details className="max-w-full"><summary className="cursor-pointer text-right">键盘操作</summary><p className="mt-1 mb-0">Tab 横向 · Enter 纵向 · Shift 扩选 · F2 编辑 · Esc 取消 · Ctrl / ⌘ C、V、D、Z、S</p></details></div>
  </section>;
}

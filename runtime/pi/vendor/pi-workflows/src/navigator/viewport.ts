/** Fits lists and long text bodies into a fixed number of terminal rows. */

import type { NavigatorState } from "../workflow-ui.js";

export interface ScrollWin {
  start: number;
  count: number;
  more: boolean;
}

/** Compute a scroll window of up to `cap` rows keeping `active` visible. */
export function scrollWindow(total: number, active: number, cap: number): ScrollWin {
  if (total <= cap) return { start: 0, count: total, more: false };
  let start = Math.max(0, Math.min(active - Math.floor(cap / 2), total - cap));
  if (active < start) start = active;
  if (active >= start + cap) start = active - cap + 1;
  return { start, count: cap, more: start + cap < total };
}

/** Which way a list continues past the rows shown. */
export type ListOverflow = "above" | "below";

/**
 * Rows for a list pane at most `cap` rows tall: a window of items around
 * `active`, with a marker row where items continue above or below it. Each
 * marker takes a row of its own, so it never hides an item and the active item
 * always shows; a pane too small for every marker keeps the one below, and a
 * one-row pane shows only the active item.
 */
export function listPaneRows(
  total: number,
  active: number,
  cap: number,
  itemRow: (index: number) => string,
  markerRow: (overflow: ListOverflow) => string,
): string[] {
  // Markers shrink the window, which can hide more items; settle on a count that holds.
  let markers = 0;
  let win = scrollWindow(total, active, cap);
  for (;;) {
    const hidden = Number(win.start > 0) + Number(win.start + win.count < total);
    const fitting = Math.min(hidden, Math.max(0, cap - 1));
    if (fitting <= markers) break;
    markers = fitting;
    win = scrollWindow(total, active, cap - markers);
  }
  const below = markers > 0 && win.start + win.count < total;
  const above = win.start > 0 && markers > Number(below);
  const rows = Array.from({ length: win.count }, (_, k) => itemRow(win.start + k));
  return [...(above ? [markerRow("above")] : []), ...rows, ...(below ? [markerRow("below")] : [])];
}

/**
 * The slice of a detail body at the state's scroll offset, plus a position
 * indicator when it overflows. The viewport height is fixed so scrolling
 * toward the end never shrinks the overlay.
 */
export function scrollableBody(
  state: NavigatorState,
  body: string[],
  viewportRows: number,
  dim: (text: string) => string,
): string[] {
  const confirmationRows = state.mode === "confirm" ? 1 : 0;
  const viewport = Math.max(1, viewportRows - 4 - confirmationRows); // title + blank + footer + indicator + confirm
  state.setPageSize(viewport);
  const maxScroll = Math.max(0, body.length - viewport);
  if (state.kind === "detail" && state.tailing) state.scroll = maxScroll;
  state.scroll = Math.min(Math.max(0, state.scroll), maxScroll);
  const out = body.slice(state.scroll, state.scroll + viewport);
  if (body.length > viewport) {
    const end = Math.min(state.scroll + viewport, body.length);
    const up = state.scroll > 0 ? "↑" : " ";
    const down = end < body.length ? "↓" : " ";
    const mode = state.kind === "detail" && state.tailing ? " TAIL" : "";
    out.push(dim(`  [${state.scroll + 1}-${end} / ${body.length}] ${up}${down}${mode}`));
  }
  return out;
}

/** The first screenful of a body. Summaries do not scroll; the full pager does. */
export function compactBody(body: string[], viewportRows: number, dim: (text: string) => string): string[] {
  const viewport = Math.max(1, viewportRows - 3); // title + blank + footer
  if (body.length <= viewport) return body;
  return [...body.slice(0, Math.max(1, viewport - 1)), dim("  … enter to open full pager")];
}

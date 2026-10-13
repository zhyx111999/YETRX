/** The top-level list: runs, then saved workflows, under a title showing the filter and scroll range. */

import type { ThemeLike } from "../display.js";
import { fmtCost, fmtFull, fmtTokenSegment, runStatusIcon } from "../display.js";
import type { NavigatorState } from "../workflow-ui.js";
import { type NavigatorSnapshot, sameIdentity, type VisibleNavigatorItem } from "./items.js";
import { type ScrollWin, scrollWindow } from "./viewport.js";

function showsBothKinds(items: VisibleNavigatorItem[], win: ScrollWin): boolean {
  const slice = items.slice(win.start, win.start + win.count);
  return slice.some((item, index) => index > 0 && item.kind !== slice[index - 1]?.kind);
}

/**
 * Every row, range, and separator comes from the one `snapshot` taken for
 * this frame, so the list stays coherent while the manager changes underneath.
 */
export function renderRunsList(
  state: NavigatorState,
  snapshot: NavigatorSnapshot,
  theme: ThemeLike,
  viewportRows: number,
): string[] {
  const dim = (t: string) => theme.fg("dim", t);
  const lines: string[] = [];
  state.reconcile(snapshot);
  const total = snapshot.items.length;
  const confirmationRows = state.mode === "confirm" || state.mode === "rename" ? 1 : 0;
  const bodyCap = Math.max(1, viewportRows - 3 - confirmationRows);
  let win = scrollWindow(total, state.cursor, bodyCap);
  // Leave a row for the "saved" separator when the window spans both kinds.
  if (showsBothKinds(snapshot.items, win) && bodyCap > 1) win = scrollWindow(total, state.cursor, bodyCap - 1);
  const windowEnd = win.start + win.count;
  const up = win.start > 0 ? "↑" : " ";
  const down = windowEnd < total ? "↓" : " ";
  const range =
    win.start > 0 || windowEnd < total ? dim(`  [${up} ${win.start + 1}-${windowEnd} / ${total} ${down}]`) : "";
  const filterLabel = state.mode === "filter" ? `  / ${state.draft}` : state.filter ? `  / ${state.filter}` : "";
  lines.push(theme.bold(`Workflows${filterLabel}${range}`));
  if (total === 0) {
    lines.push(
      dim(snapshot.filter ? "  No matching workflows." : "  No runs yet. Start one with a background workflow."),
    );
  }
  const selected = state.currentItem(snapshot)?.identity;
  const mark = (i: number, text: string) =>
    sameIdentity(snapshot.items[i]?.identity, selected) ? theme.fg("accent", theme.bold(`❯ ${text}`)) : `  ${text}`;
  for (let i = win.start; i < windowEnd; i++) {
    const item = snapshot.items[i];
    if (!item) continue;
    if (i > win.start && item.kind === "saved" && snapshot.items[i - 1]?.kind === "run")
      lines.push(dim("  ── saved ──"));
    if (item.kind === "run") {
      const row = item.row;
      const icon = runStatusIcon(row.status);
      const tok = fmtTokenSegment(row, fmtFull);
      const meta = [`${row.done}/${row.total}`, tok, row.cost > 0 ? fmtCost(row.cost) : ""].filter(Boolean).join(" · ");
      lines.push(mark(i, `${icon} ${row.name}  ${dim(`${row.runId} · ${row.status} · ${meta}`)}`));
    } else {
      const workflow = item.workflow;
      const loc = workflow.location === "user" ? "~" : ".";
      const desc = workflow.description ? dim(`  ${workflow.description}`) : "";
      lines.push(mark(i, `${workflow.name}${desc}  ${dim(loc)}`));
    }
  }
  return lines;
}

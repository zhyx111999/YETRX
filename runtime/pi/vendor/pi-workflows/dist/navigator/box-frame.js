/** Draws the run view's box borders: titled top rules and the two-pane frame with one shared divider. */
import { truncateToWidth, visibleWidth } from "@earendil-works/pi-tui";
import { ELLIPSIS } from "./format.js";
// Light box-drawing glyphs (no heavy/double variants).
export const BX = { h: "─", v: "│", tl: "┌", tr: "┐", bl: "└", br: "┘", tj: "┬", bj: "┴" };
/** Compose a titled top rule for one box side (between two join chars). */
export function topTitleSegment(title, innerW, leading, theme) {
    // leading=true → right box (one ─ before the title); leading=false → left box.
    const label = ` ${title} `;
    const lead = leading ? BX.h : "";
    let labelOut = label;
    const fixed = visibleWidth(lead) + 1; // + at least one trailing dash
    if (visibleWidth(label) > innerW - fixed) {
        labelOut = truncateToWidth(label, Math.max(0, innerW - fixed), ELLIPSIS, false);
    }
    const used = visibleWidth(lead) + visibleWidth(labelOut);
    const dashes = BX.h.repeat(Math.max(0, innerW - used));
    return theme.fg("muted", lead) + theme.fg("dim", labelOut) + theme.fg("muted", dashes);
}
/** Emit the full combined frame (top rule, body rows, bottom rule). */
export function renderTwoPaneFrame(a) {
    const { width, bodyRows, left, right, leftTitle, rightTitle, leftW, theme } = a;
    // RW fills the remainder; the divider column is shared (overlaps 1 cell) so
    // net rendered width = LW + RW - 1 = width. Hence RW = width - LW + 1.
    const rightW = width - leftW + 1;
    const leftInner = leftW - 2;
    const rightInner = rightW - 2;
    const bc = (s) => theme.fg("muted", s);
    const out = [];
    // Top rule: ┌ <left title> ┬ <right title> ┐
    out.push(bc(BX.tl) +
        topTitleSegment(leftTitle, leftInner, false, theme) +
        bc(BX.tj) +
        topTitleSegment(rightTitle, rightInner, true, theme) +
        bc(BX.tr));
    // Body rows.
    const blankL = " ".repeat(leftInner);
    const blankR = " ".repeat(rightInner);
    for (let r = 0; r < bodyRows; r++) {
        const l = left[r] ?? blankL;
        const rr = right[r] ?? blankR;
        out.push(bc(BX.v) + l + bc(BX.v) + rr + bc(BX.v));
    }
    // Bottom rule: └ ─ ┴ ─ ┘
    out.push(bc(BX.bl) + bc(BX.h.repeat(leftInner)) + bc(BX.bj) + bc(BX.h.repeat(rightInner)) + bc(BX.br));
    return out;
}

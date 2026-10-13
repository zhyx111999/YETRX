/** The navigator overlay's size and its rounded, background-filled border. */

import type { Theme } from "@earendil-works/pi-coding-agent";
import type { OverlayOptions } from "@earendil-works/pi-tui";
import { truncateToWidth } from "@earendil-works/pi-tui";

const WIDTH_PERCENT = 94;
const MAX_HEIGHT_PERCENT = 92;
const MARGIN = 1;

const TITLE = " workflows ";
const BORDER_LEFT = "│ ";
const BORDER_RIGHT = " │";
const FRAME_COLS = BORDER_LEFT.length + BORDER_RIGHT.length;
const FRAME_ROWS = 2; // top + bottom border
/** Two corners, one leading dash, and the title: the narrowest top border that keeps the title whole. */
const MIN_FRAMED_WIDTH = 3 + TITLE.length;

/** A roomy overlay with a margin, so its border stands out from the terminal edge. */
export function overlayOptions(): OverlayOptions {
  return { width: `${WIDTH_PERCENT}%`, maxHeight: `${MAX_HEIGHT_PERCENT}%`, anchor: "center", margin: MARGIN };
}

/** Where navigator content goes inside the overlay, and whether the border fits around it. */
export interface OverlayLayout {
  /** Overlay columns, as allocated by the host. */
  width: number;
  framed: boolean;
  contentWidth: number;
  contentRows: number;
}

/**
 * Rows the host lets the overlay draw. pi-tui takes the maxHeight share of the
 * terminal rows, clamps it to the rows inside the top and bottom margins, and
 * drops every line past that, so a taller overlay loses its footer and bottom
 * border.
 */
function overlayRows(terminalRows: number): number {
  const share = Math.floor((terminalRows * MAX_HEIGHT_PERCENT) / 100);
  return Math.max(1, Math.min(share, terminalRows - 2 * MARGIN));
}

/**
 * Lay the navigator out in the host's allocation: `width` is what the host
 * passed to render, already clamped to its width option and margins. The
 * border is drawn only when it fits whole with at least one content row;
 * otherwise the content takes the whole overlay.
 */
export function overlayLayout(width: number, terminalRows: number): OverlayLayout {
  const rows = overlayRows(terminalRows);
  if (width >= MIN_FRAMED_WIDTH && rows > FRAME_ROWS) {
    return { width, framed: true, contentWidth: width - FRAME_COLS, contentRows: rows - FRAME_ROWS };
  }
  return { width, framed: false, contentWidth: width, contentRows: rows };
}

/**
 * Fit content lines into the layout, inside the border when it is framed; a
 * brighter border marks the focused overlay. Every row is padded to the full
 * width so the background colour spans the overlay evenly.
 */
export function boxOverlayLines(lines: string[], layout: OverlayLayout, theme: Theme, focused: boolean): string[] {
  const { width, contentWidth } = layout;
  const bg = (s: string) => theme.bg("customMessageBg", s);
  const fit = (line: string) => truncateToWidth(line, contentWidth, "", true);
  const body = lines.slice(0, layout.contentRows);
  if (!layout.framed) return body.map((line) => bg(fit(line)));

  const border = (s: string) => (focused ? theme.fg("accent", s) : theme.fg("borderMuted", s));
  const title = focused ? theme.fg("dim", theme.bold(TITLE)) : theme.fg("muted", TITLE);
  const top = border("╭─") + title + border(`${"─".repeat(width - MIN_FRAMED_WIDTH)}╮`);
  const bottom = border(`╰${"─".repeat(width - 2)}╯`);
  const boxed = (line: string) => border(BORDER_LEFT) + fit(line) + border(BORDER_RIGHT);
  return [bg(top), ...body.map((line) => bg(boxed(line))), bg(bottom)];
}

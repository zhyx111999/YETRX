/** Renders prompts, results, source code, and edit diffs as width-wrapped terminal lines. */

import { renderDiff } from "@earendil-works/pi-coding-agent";
import type { MarkdownTheme } from "@earendil-works/pi-tui";
import { Markdown, wrapTextWithAnsi } from "@earendil-works/pi-tui";
import { asText } from "./as-text.js";
import type { NavigatorTextRenderCache } from "./render-cache.js";

function cachedLines(renderCache: NavigatorTextRenderCache | undefined, key: string, render: () => string[]): string[] {
  const cached = renderCache?.get(key);
  if (cached) return cached;
  const lines = render();
  return renderCache?.set(key, lines, key.length + lines.reduce((sum, line) => sum + line.length, 0)) ?? lines;
}

/** Render prose as Markdown when the host theme is available. Fenced code blocks
 * are syntax highlighted by pi's Markdown renderer. */
export function renderMarkdownLines(
  text: unknown,
  width: number,
  markdownTheme?: MarkdownTheme,
  renderCache?: NavigatorTextRenderCache,
): string[] {
  const safeText = asText(text);
  if (!markdownTheme) return wrapTextWithAnsi(safeText, Math.max(1, width));
  const renderWidth = Math.max(1, width);
  return cachedLines(renderCache, `md:${renderWidth}:${safeText}`, () =>
    new Markdown(safeText, 0, 0, markdownTheme).render(renderWidth),
  );
}

/** Render Pi's display-oriented edit diff inside the navigator's bounded
 * viewport while preserving its ANSI colors and intra-line highlights. */
export function renderDiffLines(diff: string, width: number, renderCache?: NavigatorTextRenderCache): string[] {
  const renderWidth = Math.max(1, width);
  return cachedLines(renderCache, `diff:${renderWidth}:${diff}`, () =>
    renderDiff(diff)
      .split("\n")
      .flatMap((line) => wrapTextWithAnsi(`  ${line}`, renderWidth)),
  );
}

/** Render a known-language source block without requiring Markdown fences (a
 * workflow script can itself contain backticks). */
export function renderCodeLines(
  text: unknown,
  language: string,
  width: number,
  markdownTheme?: MarkdownTheme,
  renderCache?: NavigatorTextRenderCache,
): string[] {
  const safeText = asText(text);
  const renderWidth = Math.max(1, width);
  return cachedLines(renderCache, `code:${language}:${renderWidth}:${safeText}`, () => {
    const sourceLines = markdownTheme?.highlightCode?.(safeText, language) ?? safeText.split("\n");
    return sourceLines.flatMap((line) => wrapTextWithAnsi(`  ${line}`, renderWidth));
  });
}

/** Structured results render as highlighted JSON; text results as Markdown. */
export function renderResultLines(
  result: unknown,
  preview: string | undefined,
  width: number,
  markdownTheme?: MarkdownTheme,
  renderCache?: NavigatorTextRenderCache,
): string[] {
  if (result !== undefined && typeof result !== "string") {
    let json: string;
    if (renderCache && typeof result === "object" && result !== null) {
      json = renderCache.stringify(result);
    } else {
      try {
        json = JSON.stringify(result, null, 2) ?? String(result);
      } catch {
        json = String(result);
      }
    }
    return renderCodeLines(json, "json", width, markdownTheme, renderCache);
  }
  return renderMarkdownLines(
    typeof result === "string" ? result : (preview ?? "(none)"),
    width,
    markdownTheme,
    renderCache,
  );
}

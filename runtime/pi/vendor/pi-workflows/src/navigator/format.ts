/** Model and phase labels, plurals, and truncation shared by navigator cells. */

import { asText } from "./as-text.js";

/** Marks text cut to fit its cell. */
export const ELLIPSIS = "…";

export function pluralize(word: string, n: number): string {
  return n === 1 ? word : `${word}s`;
}

/** Short, human-friendly model label: drop the provider prefix for display. */
export function shortModel(model: string | undefined): string | undefined {
  if (!model) return undefined;
  const m = asText(model);
  const slash = m.indexOf("/");
  return slash > 0 ? m.slice(slash + 1) : m;
}

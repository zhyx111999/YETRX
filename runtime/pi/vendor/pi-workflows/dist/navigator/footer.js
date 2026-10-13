/** The rows under every view: a pending confirm or rename prompt, a blank line, and the key hints. */
import { confirmationPrompt } from "./confirmation.js";
export function footerLines(state, model, theme, snapshot) {
    const lines = [];
    const confirmation = state.confirmation;
    if (state.mode === "confirm" && confirmation) {
        lines.push(theme.fg("warning", `  ${confirmationPrompt(confirmation)}`));
    }
    else if (state.mode === "rename") {
        lines.push(theme.fg("accent", `  Rename: ${state.draft}`));
    }
    lines.push("", keyHints(state, model, theme, snapshot));
    return lines;
}
function keyHints(state, model, theme, snapshot) {
    const parts = [];
    switch (state.kind) {
        case "detail":
            if (state.pagerOpen) {
                parts.push("↑/↓ line", "PgUp/PgDn page", "g/G ends", `t tail:${state.tailing ? "on" : "off"}`, "enter summary", "esc back");
            }
            else {
                parts.push("enter open pager", "t tail", "esc back");
            }
            break;
        case "savedDetail":
            parts.push("↑/↓ line", "PgUp/PgDn page", "g/G ends", "esc back", "r rename", "x delete");
            break;
        case "runs": {
            const item = state.currentItem(snapshot ?? model.visible(state.filter));
            parts.push("↑/↓ select", "/ filter", "enter open", "esc back");
            if (item?.kind === "run") {
                parts.push("p pause", "x stop", "r restart", "s save");
            }
            else if (item?.kind === "saved") {
                parts.push("r rename", "x delete");
            }
            parts.push("q quit");
            break;
        }
        default:
            parts.push("↑/↓ select", "enter open", "esc back", "q quit");
    }
    return theme.fg("dim", parts.join(" · "));
}

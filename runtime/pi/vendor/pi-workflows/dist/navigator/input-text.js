/** Turns terminal input for the navigator's filter and rename fields into the text it types. */
import { decodeKittyPrintable } from "@earendil-works/pi-tui";
/**
 * Escape sequences a terminal sends, matched whole so no tail is left behind:
 * - CSI: parameter and intermediate bytes, then one final byte (ESC [ 1 ; 5 A).
 * - OSC, DCS, SOS, PM, APC: through BEL or ST. An unterminated one runs to the
 *   end, so a title or payload never reaches the field.
 * - SS2/SS3 and the one character they shift (ESC O A is an arrow key).
 * - Any other escape or Alt chord: intermediates, then the character they modify.
 * An ESC before a control character matches nothing here and is removed with
 * the other control characters, so ESC ESC [ A still drops the whole arrow.
 */
const ESCAPE_SEQUENCE = new RegExp([
    String.raw `\x1b\[[\x20-\x3f]*[\x40-\x7e]?`,
    String.raw `\x1b[\]PX^_][\s\S]*?(?:\x07|\x1b\\|$)`,
    String.raw `\x1b[NO][\x20-\x7e]?`,
    String.raw `\x1b[\x20-\x2f]*[^\p{Cc}]`,
].join("|"), "gu");
/** C0, DEL, and C1. Format characters (ZWJ, emoji tags) and marks (VS16) are text. */
const CONTROL_CHARACTER = /\p{Cc}/gu;
/**
 * Remove escape sequences and control characters from typed or pasted text and
 * keep everything else, so a filter can match any name exactly. Whether a name
 * may become a slash command is checked when it is saved or renamed, not here.
 */
export function safeInputText(data) {
    return data.replace(ESCAPE_SEQUENCE, "").replace(CONTROL_CHARACTER, "");
}
/**
 * The text one input event types. `key` is pi-tui's `parseKey` result for the
 * same bytes. A named key types only when its bytes are already plain text
 * ("a", a space); arrows, function keys, and Ctrl or Alt chords type nothing,
 * whatever sanitizing would leave of them. Kitty's CSI-u form of a plain
 * character is the one escape sequence that carries text.
 */
export function typedText(data, key) {
    const kittyText = decodeKittyPrintable(data);
    if (kittyText !== undefined)
        return safeInputText(kittyText);
    const text = safeInputText(data);
    return key === undefined || text === data ? text : "";
}
/** Backspace by one user-perceived character, so emoji and combining marks go in one keypress. */
export function dropLastGrapheme(text) {
    const Segmenter = Intl.Segmenter;
    const graphemes = Segmenter ? [...new Segmenter().segment(text)].map((entry) => entry.segment) : Array.from(text);
    return graphemes.slice(0, -1).join("");
}

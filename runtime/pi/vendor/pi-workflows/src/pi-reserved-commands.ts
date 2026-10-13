/**
 * Slash commands Pi's interactive editor runs itself. The editor matches these
 * before it looks up extension commands, yet `pi.getCommands()` does not list
 * them and `pi.registerCommand()` does not reject them, so an extension command
 * with one of these names registers fine and never runs.
 *
 * The SDK does not export this list: `BUILTIN_SLASH_COMMANDS` lives in
 * `dist/core/slash-commands.js`, which the package's `exports` map hides, and
 * `InteractiveMode.setupEditorSubmitHandler` matches a few more names
 * (`dist/modes/interactive/interactive-mode.js`). The set is the union over
 * the Pi releases this package supports, 0.84.0 through 1.1.0, so a name any
 * of them reserves is refused. `tests/pi-reserved-commands.test.ts` fails when
 * the installed SDK reserves a name missing here.
 */
export const PI_RESERVED_COMMANDS: ReadonlySet<string> = new Set([
  // BUILTIN_SLASH_COMMANDS
  "settings",
  "model",
  "tree",
  // Since 0.84.3
  "thinking",
  "scoped-models",
  "export",
  "import",
  "share",
  // Since 0.86.0
  "bug",
  "copy",
  "name",
  "session",
  "changelog",
  "hotkeys",
  "fork",
  "clone",
  "trust",
  "login",
  "logout",
  "new",
  "compact",
  "resume",
  "reload",
  "quit",
  // Matched by the editor, absent from autocomplete
  "debug",
  "arminsayshi",
  "dementedelves",
]);

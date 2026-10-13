// Pi's extension loader evaluates this file and supplies its own packages for
// the imports below. The compiled graph in dist/ is loaded with Node's import
// instead, so before the first dist/ module loads, bindHostModules routes the
// graph's imports of these packages to the same modules: the extension runs on
// the Pi that loaded it, never on copies in this package's node_modules.
// Node keeps that graph for the life of the process: /reload re-runs the
// extension with the code already loaded, and an updated build loads when Pi
// restarts (the extension warns when the package on disk has changed).
import * as codingAgent from "@earendil-works/pi-coding-agent";
import * as tui from "@earendil-works/pi-tui";
import * as typebox from "typebox";
import * as typeboxValue from "typebox/value";

const { bindHostModules } = await import("../dist/host-modules.js");
bindHostModules(new URL("../dist/", import.meta.url), {
  "@earendil-works/pi-coding-agent": codingAgent,
  "@earendil-works/pi-tui": tui,
  typebox,
  "typebox/value": typeboxValue,
});
const extension = await import("../dist/pi-extension.js");
export default extension.default;

/**
 * Hands this package's compiled modules the Pi packages the running Pi
 * supplies, instead of copies Node would find in the package's own
 * node_modules.
 *
 * Pi maps its packages (pi-coding-agent, pi-tui, typebox) only for code its
 * extension loader evaluates itself. It imports the compiled modules in dist/
 * with Node's own import, which resolves those names from the package
 * directory, so a package that has its own copies installed (a development
 * checkout) would run on them: a second SDK of another version, with its own
 * classes, beside the one Pi runs. A module hook routes those imports, from
 * dist/ only, to the modules the extension entry received from Pi.
 */
import { registerHooks } from "node:module";
/** The Pi packages the compiled modules import at run time. */
export const HOST_PACKAGES = [
    "@earendil-works/pi-coding-agent",
    "@earendil-works/pi-tui",
    "typebox",
    "typebox/value",
];
const BRIDGE_SCHEME = "pi-workflows-lean-host:";
/** Process-wide, so every extension generation and installed copy shares one hook. */
const STATE_KEY = Symbol.for("pi-workflows-lean.host-modules");
function bridgeState() {
    return globalThis[STATE_KEY];
}
/**
 * Route the host packages imported by modules under `distRoot` to `modules`.
 * Call before the first import of a module under `distRoot`. A later call (a
 * reload, another installed copy) adds its root and hands in the current
 * modules; one hook serves them all.
 */
export function bindHostModules(distRoot, modules) {
    const root = distRoot.href.endsWith("/") ? distRoot.href : `${distRoot.href}/`;
    const existing = bridgeState();
    if (existing) {
        existing.modules = modules;
        existing.roots.add(root);
        return;
    }
    const state = { modules, roots: new Set([root]) };
    globalThis[STATE_KEY] = state;
    registerHooks({
        resolve(specifier, context, nextResolve) {
            const parent = context.parentURL;
            if (isHostPackage(specifier) && parent && [...state.roots].some((r) => parent.startsWith(r))) {
                return { url: `${BRIDGE_SCHEME}${specifier}`, shortCircuit: true };
            }
            return nextResolve(specifier, context);
        },
        load(url, context, nextLoad) {
            if (!url.startsWith(BRIDGE_SCHEME))
                return nextLoad(url, context);
            return {
                format: "module",
                source: bridgeSource(url.slice(BRIDGE_SCHEME.length)),
                shortCircuit: true,
            };
        },
    });
}
function isHostPackage(specifier) {
    return HOST_PACKAGES.includes(specifier);
}
const IDENTIFIER = /^[A-Za-z_$][\w$]*$/;
/** An ES module that re-exports one host module's current exports. */
function bridgeSource(name) {
    const module = bridgeState()?.modules[name];
    if (!module)
        throw new Error(`pi-workflows-lean: Pi did not supply ${name}`);
    const access = `globalThis[Symbol.for(${JSON.stringify(STATE_KEY.description)})].modules[${JSON.stringify(name)}]`;
    const names = Object.keys(module).filter((key) => key !== "default" && IDENTIFIER.test(key));
    return [
        `const host = ${access};`,
        ...names.map((key) => `export const ${key} = host[${JSON.stringify(key)}];`),
        "default" in module ? "export default host.default;" : "",
    ].join("\n");
}

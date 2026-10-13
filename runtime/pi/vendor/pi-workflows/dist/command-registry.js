import { PI_RESERVED_COMMANDS } from "./pi-reserved-commands.js";
const commandOwnersKey = Symbol.for("pi-dynamic-workflows.command-owners");
const fallbackRegistries = new WeakMap();
function registryFor(pi) {
    const carrier = pi;
    const existing = carrier[commandOwnersKey];
    if (existing)
        return existing;
    const registry = new Map();
    try {
        Object.defineProperty(carrier, commandOwnersKey, {
            configurable: false,
            enumerable: false,
            value: registry,
            writable: false,
        });
        return registry;
    }
    catch {
        // A frozen or host-provided API object cannot carry the reload-stable symbol;
        // retain ownership for the lifetime of this API object as a safe fallback.
        const fallback = fallbackRegistries.get(pi);
        if (fallback)
            return fallback;
        fallbackRegistries.set(pi, registry);
        return registry;
    }
}
/**
 * Whether `/<name>` already reaches a handler: one of Pi's own commands, which
 * the host never lists, or an extension, prompt, or skill command it does list.
 */
export function isCommandTaken(pi, name) {
    if (PI_RESERVED_COMMANDS.has(name))
        return true;
    try {
        return (pi.getCommands?.() ?? []).some((command) => command.name === name);
    }
    catch {
        return false;
    }
}
export function commandOwner(pi, name) {
    return registryFor(pi).get(name);
}
/** Claim a command only after this extension has successfully registered it. */
export function claimCommand(pi, name, owner) {
    const registry = registryFor(pi);
    const current = registry.get(name);
    if (current && current !== owner)
        return false;
    registry.set(name, owner);
    return true;
}

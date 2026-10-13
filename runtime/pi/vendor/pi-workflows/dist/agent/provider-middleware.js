const RECURSIVE_SUBAGENT_EXTENSION_NAMES = new Set(["pi-dynamic-workflows", "workflow", "pi-subagents"]);
/**
 * Keep only explicitly approved provider/auth middleware paths. Recursive
 * orchestration extensions are always rejected, even if explicitly allowlisted.
 */
export function isProviderMiddlewareExtensionPath(extensionPath, allowlist, packageSource) {
    const allowed = new Set(allowlist.map((name) => name.trim().toLowerCase()).filter(Boolean));
    const segments = extensionPath
        .replaceAll("\\", "/")
        .split("/")
        .filter(Boolean)
        .map((segment) => segment.toLowerCase());
    const file = segments.at(-1)?.replace(/\.(?:[cm]?[jt]s)$/i, "");
    if ([...segments, file ?? ""].some((name) => RECURSIVE_SUBAGENT_EXTENSION_NAMES.has(name)))
        return false;
    // Ancestor directory names are not extension identities: an allowlisted
    // name appearing in a project/home path must not approve all descendants.
    const identities = new Set(file ? [file] : []);
    const moduleIndex = segments.lastIndexOf("node_modules");
    const packageName = segments[moduleIndex + 1];
    if (moduleIndex >= 0 && packageName) {
        identities.add(packageName.startsWith("@") ? `${packageName}/${segments[moduleIndex + 2] ?? ""}` : packageName);
    }
    if (packageSource) {
        const source = packageSource.replaceAll("\\", "/").toLowerCase();
        const npmName = /^npm:((?:@[^/]+\/)?[^@]+)(?:@.*)?$/.exec(source)?.[1];
        const sourceName = npmName ??
            source
                .replace(/[?#].*$/, "")
                .replace(/\/+$/, "")
                .split("/")
                .at(-1)
                ?.replace(/\.git$/, "");
        if (sourceName)
            identities.add(sourceName);
    }
    if ([...identities].some((name) => RECURSIVE_SUBAGENT_EXTENSION_NAMES.has(name)))
        return false;
    return [...identities].some((name) => allowed.has(name));
}
/** Drop every loaded extension that isProviderMiddlewareExtensionPath does not approve. */
export function filterProviderMiddlewareExtensions(base, allowlist, packageSources = new Map()) {
    return {
        ...base,
        extensions: base.extensions.filter((extension) => isProviderMiddlewareExtensionPath(extension.path, allowlist, packageSources.get(extension.path))),
    };
}

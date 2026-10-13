// Builds child-session resource loaders: one shared per directory within a run, with opt-in provider middleware only.
import { DefaultPackageManager, DefaultResourceLoader, SettingsManager, } from "@earendil-works/pi-coding-agent";
import { filterProviderMiddlewareExtensions, isProviderMiddlewareExtensionPath } from "./provider-middleware.js";
/**
 * Worktree isolation gives every agent its own cwd, so without a cap N worktree
 * agents would keep N fully loaded loaders until the run ends. Hits re-insert
 * their entry (LRU by touch), so the base cwd stays resident while one-off
 * worktree loaders are evicted first.
 */
const MAX_SHARED_RESOURCE_LOADERS = 8;
/**
 * Without a resourceLoader, createAgentSession() builds a fresh loader per
 * subagent and re-runs every installed extension factory, which is the memory
 * churn this cache avoids.
 *
 * Host extensions stay disabled by default. When provider middleware is opted
 * in, configured paths are resolved without loading factories and only the
 * allowlisted ones are added to a `noExtensions: true` loader, so recursive
 * orchestration factories never load. Skills, prompts, AGENTS.md context, and
 * workflow-supplied `customTools` stay available. Allowlisted middleware must be
 * trusted and child-safe; this is not a sandbox.
 *
 * Extension-free loaders are shared per directory. Middleware loaders are built
 * per session: the SDK binds session actions into their runtime, so sharing one
 * would send a child's extension actions into another child.
 */
export class SharedResourceLoaders {
    loaders = new Map();
    middleware;
    /** Read when a loader is built, so an injected settingsManager is honored. */
    sessionOptions;
    constructor(middleware, sessionOptions) {
        this.middleware = middleware;
        this.sessionOptions = sessionOptions;
    }
    /** Number of memoized loaders. */
    get size() {
        return this.loaders.size;
    }
    get(agentDir, cwd) {
        const key = JSON.stringify([agentDir, cwd]);
        const existing = this.shared ? this.loaders.get(key) : undefined;
        if (existing) {
            // Touch: move the hit to the newest position so pruning evicts colder entries first.
            this.loaders.delete(key);
            this.loaders.set(key, existing);
            return existing;
        }
        return this.build(agentDir, cwd, key);
    }
    get shared() {
        return this.middleware.length === 0;
    }
    build(agentDir, cwd, key) {
        const shared = this.shared;
        const pending = (async () => {
            const settingsManager = this.sessionOptions.settingsManager ?? SettingsManager.create(cwd, agentDir);
            let middlewarePaths = [];
            const packageSources = new Map();
            if (this.middleware.length > 0) {
                await settingsManager.reload();
                const packageManager = new DefaultPackageManager({ cwd, agentDir, settingsManager });
                const configured = await packageManager.resolve();
                middlewarePaths = configured.extensions
                    .filter((extension) => extension.enabled)
                    .filter((extension) => {
                    const source = extension.metadata.origin === "package" ? extension.metadata.source : undefined;
                    if (source)
                        packageSources.set(extension.path, source);
                    return isProviderMiddlewareExtensionPath(extension.path, this.middleware, source);
                })
                    .map((extension) => extension.path);
            }
            const loader = new DefaultResourceLoader({
                cwd,
                agentDir,
                settingsManager,
                noExtensions: true,
                additionalExtensionPaths: middlewarePaths,
                extensionsOverride: (base) => filterProviderMiddlewareExtensions(base, this.middleware, packageSources),
            });
            await loader.reload();
            return loader;
        })().catch((err) => {
            // Don't let a transient build failure (e.g. EMFILE during reload's disk
            // I/O) poison every subagent and every retry of this run: clear the memo
            // so the next caller rebuilds instead of replaying the same rejection.
            // An evicted older build must not delete a newer entry for this key.
            if (shared && this.loaders.get(key) === pending)
                this.loaders.delete(key);
            throw err;
        });
        if (shared) {
            this.loaders.set(key, pending);
            this.prune();
        }
        return pending;
    }
    prune() {
        while (this.loaders.size > MAX_SHARED_RESOURCE_LOADERS) {
            const oldest = this.loaders.keys().next().value;
            if (oldest === undefined)
                return;
            this.loaders.delete(oldest);
        }
    }
}

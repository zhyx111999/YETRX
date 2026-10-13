/**
 * Keeps a rejection that a workflow script itself leaves unhandled, such as a
 * bare Promise.reject() or a throwing async function it never awaits, from
 * reaching Node's default handling, which raises it as an uncaught exception
 * and ends the Pi process. Promises the workflow globals return are tracked in
 * script-calls.ts; these are the ones the script makes on its own.
 */
/** A realm whose run has ended: its late rejections are still kept from the process, and ignored. */
const ENDED = { rejected: () => { }, handled: () => { } };
/** The reporter of each script realm, keyed by that realm's own Promise.prototype. */
const realms = new WeakMap();
let installed = false;
function realmOf(promise) {
    return realms.get(Object.getPrototypeOf(promise));
}
function onUnhandledRejection(reason, promise) {
    const realm = realmOf(promise);
    if (realm) {
        realm.rejected(promise, reason);
        return;
    }
    // Not a workflow script's promise. Without listeners Node raises it as an
    // uncaught exception; while this is the only listener, keep doing exactly that.
    if (process.listenerCount("unhandledRejection") === 1)
        throw reason;
}
function onRejectionHandled(promise) {
    realmOf(promise)?.handled(promise);
}
/**
 * Route the unhandled rejections of the realm whose Promise.prototype is
 * `realmPromisePrototype` to `rejections`. Returns the function that stops
 * reporting them; the realm's later rejections are still kept from the process.
 */
export function routeRealmRejections(realmPromisePrototype, rejections) {
    if (!installed) {
        process.on("unhandledRejection", onUnhandledRejection);
        process.on("rejectionHandled", onRejectionHandled);
        installed = true;
    }
    realms.set(realmPromisePrototype, rejections);
    return () => realms.set(realmPromisePrototype, ENDED);
}

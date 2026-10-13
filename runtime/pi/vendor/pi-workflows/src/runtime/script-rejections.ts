/**
 * Keeps a rejection that a workflow script itself leaves unhandled, such as a
 * bare Promise.reject() or a throwing async function it never awaits, from
 * reaching Node's default handling, which raises it as an uncaught exception
 * and ends the Pi process. Promises the workflow globals return are tracked in
 * script-calls.ts; these are the ones the script makes on its own.
 */

/** Receives a script promise that rejected with no handler, and the same promise once a handler arrives. */
export interface RealmRejections {
  rejected(promise: Promise<unknown>, reason: unknown): void;
  handled(promise: Promise<unknown>): void;
}

/** A realm whose run has ended: its late rejections are still kept from the process, and ignored. */
const ENDED: RealmRejections = { rejected: () => {}, handled: () => {} };

/** The reporter of each script realm, keyed by that realm's own Promise.prototype. */
const realms = new WeakMap<object, RealmRejections>();
let installed = false;

function realmOf(promise: Promise<unknown>): RealmRejections | undefined {
  return realms.get(Object.getPrototypeOf(promise));
}

function onUnhandledRejection(reason: unknown, promise: Promise<unknown>): void {
  const realm = realmOf(promise);
  if (realm) {
    realm.rejected(promise, reason);
    return;
  }
  // Not a workflow script's promise. Without listeners Node raises it as an
  // uncaught exception; while this is the only listener, keep doing exactly that.
  if (process.listenerCount("unhandledRejection") === 1) throw reason;
}

function onRejectionHandled(promise: Promise<unknown>): void {
  realmOf(promise)?.handled(promise);
}

/**
 * Route the unhandled rejections of the realm whose Promise.prototype is
 * `realmPromisePrototype` to `rejections`. Returns the function that stops
 * reporting them; the realm's later rejections are still kept from the process.
 */
export function routeRealmRejections(realmPromisePrototype: object, rejections: RealmRejections): () => void {
  if (!installed) {
    process.on("unhandledRejection", onUnhandledRejection);
    process.on("rejectionHandled", onRejectionHandled);
    installed = true;
  }
  realms.set(realmPromisePrototype, rejections);
  return () => realms.set(realmPromisePrototype, ENDED);
}

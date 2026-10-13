/**
 * Tracks every promise the workflow globals hand to the script. The run drains
 * them before teardown, none of their rejections can reach the host process as
 * an unhandled rejection, and the failures the script never observed are kept
 * for the teardown to settle (run-teardown.ts).
 */
/** Tracked promises the script attached a handler to: await, then, catch, finally or a Promise combinator. */
const observedPromises = new WeakSet();
/**
 * The promise type the script receives. Node exposes no per-promise "handled"
 * flag, so then() is where the script reveals that it handles a call: its
 * constructor is never the awaiting realm's own Promise, so every await,
 * catch(), finally() and Promise combinator calls then(). The derived promise
 * is tracked as well, so a chain the script never handles still settles here
 * instead of reaching the host process.
 */
class ScriptPromise extends Promise {
    // then() tracks its own derived promise; the default species would construct an untracked ScriptPromise.
    static get [Symbol.species]() {
        return Promise;
    }
    #track;
    constructor(executor, track) {
        super(executor);
        this.#track = track;
    }
    // biome-ignore lint/suspicious/noThenProperty: overriding then() is how the run learns that the script handles this promise.
    then(onFulfilled, onRejected) {
        observedPromises.add(this);
        return this.#track(super.then(onFulfilled, onRejected));
    }
}
export function createScriptCalls() {
    return { pending: new Set(), failures: [], ownRejections: new Map() };
}
/**
 * Route a promise-returning workflow global through the run's tracker. Every
 * global that returns a promise must be exposed through this, so a call the
 * script forgets to await is still drained and settled. A synchronous throw
 * (argument validation) becomes a rejection, as in an async global.
 */
export function trackScriptCalls(calls, global, start) {
    return (...args) => {
        let call;
        try {
            call = start(...args);
        }
        catch (error) {
            call = Promise.reject(error);
        }
        return track(calls, global, call);
    };
}
/** The failures whose promise the script never handled, in the order they happened. Final once nothing is pending. */
export function unobservedFailures(calls) {
    const fromGlobals = calls.failures
        .filter(({ promise }) => !observedPromises.has(promise))
        .map(({ global, error }) => ({ global, error }));
    const own = [...calls.ownRejections.values()].map((error) => ({ error }));
    return [...fromGlobals, ...own];
}
function track(calls, global, source) {
    const promise = new ScriptPromise((resolve) => resolve(source), (derived) => track(calls, global, derived));
    // The intrinsic then() does not mark the promise observed, and its rejection
    // handler is what keeps an unhandled call away from the host process. The
    // script may still attach a handler later, so observation is checked again
    // when the run settles.
    const settled = Promise.prototype.then.call(promise, () => {
        calls.pending.delete(settled);
    }, (error) => {
        calls.pending.delete(settled);
        if (!observedPromises.has(promise))
            calls.failures.push({ global, error, promise });
    });
    calls.pending.add(settled);
    return promise;
}

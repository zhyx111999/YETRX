/**
 * Tracks every promise the workflow globals hand to the script. The run drains
 * them before teardown, none of their rejections can reach the host process as
 * an unhandled rejection, and the failures the script never observed are kept
 * for the teardown to settle (run-teardown.ts).
 */

/** One run's tracked promises. */
export interface ScriptCalls {
  /** One entry per tracked promise that has not settled; the teardown drains them. */
  readonly pending: Set<Promise<unknown>>;
  /** Rejections that had no script handler yet when they happened, in order. */
  readonly failures: TrackedFailure[];
  /** Promises the script created itself that rejected and are still unhandled (see script-rejections.ts). */
  readonly ownRejections: Map<Promise<unknown>, unknown>;
}

interface TrackedFailure {
  readonly global: string;
  readonly error: unknown;
  readonly promise: Promise<unknown>;
}

/** A rejection the script never attached a handler to. */
export interface UnobservedFailure {
  /** The workflow global whose promise, or a promise derived from it, rejected; absent for a promise the script made itself. */
  readonly global?: string;
  readonly error: unknown;
}

/** Tracked promises the script attached a handler to: await, then, catch, finally or a Promise combinator. */
const observedPromises = new WeakSet<Promise<unknown>>();

/**
 * The promise type the script receives. Node exposes no per-promise "handled"
 * flag, so then() is where the script reveals that it handles a call: its
 * constructor is never the awaiting realm's own Promise, so every await,
 * catch(), finally() and Promise combinator calls then(). The derived promise
 * is tracked as well, so a chain the script never handles still settles here
 * instead of reaching the host process.
 */
class ScriptPromise extends Promise<unknown> {
  // then() tracks its own derived promise; the default species would construct an untracked ScriptPromise.
  static override get [Symbol.species]() {
    return Promise;
  }

  readonly #track: (derived: Promise<unknown>) => Promise<unknown>;

  constructor(
    executor: (resolve: (value: unknown) => void, reject: (reason?: unknown) => void) => void,
    track: (derived: Promise<unknown>) => Promise<unknown>,
  ) {
    super(executor);
    this.#track = track;
  }

  // biome-ignore lint/suspicious/noThenProperty: overriding then() is how the run learns that the script handles this promise.
  override then<TResult1 = unknown, TResult2 = never>(
    onFulfilled?: ((value: unknown) => TResult1 | PromiseLike<TResult1>) | null,
    onRejected?: ((reason: unknown) => TResult2 | PromiseLike<TResult2>) | null,
  ): Promise<TResult1 | TResult2> {
    observedPromises.add(this);
    return this.#track(super.then(onFulfilled, onRejected)) as Promise<TResult1 | TResult2>;
  }
}

export function createScriptCalls(): ScriptCalls {
  return { pending: new Set<Promise<unknown>>(), failures: [], ownRejections: new Map() };
}

/**
 * Route a promise-returning workflow global through the run's tracker. Every
 * global that returns a promise must be exposed through this, so a call the
 * script forgets to await is still drained and settled. A synchronous throw
 * (argument validation) becomes a rejection, as in an async global.
 */
export function trackScriptCalls<A extends unknown[]>(
  calls: ScriptCalls,
  global: string,
  start: (...args: A) => Promise<unknown>,
): (...args: A) => Promise<unknown> {
  return (...args: A) => {
    let call: Promise<unknown>;
    try {
      call = start(...args);
    } catch (error) {
      call = Promise.reject(error);
    }
    return track(calls, global, call);
  };
}

/** The failures whose promise the script never handled, in the order they happened. Final once nothing is pending. */
export function unobservedFailures(calls: ScriptCalls): UnobservedFailure[] {
  const fromGlobals = calls.failures
    .filter(({ promise }) => !observedPromises.has(promise))
    .map(({ global, error }) => ({ global, error }));
  const own = [...calls.ownRejections.values()].map((error) => ({ error }));
  return [...fromGlobals, ...own];
}

function track(calls: ScriptCalls, global: string, source: Promise<unknown>): Promise<unknown> {
  const promise = new ScriptPromise(
    (resolve) => resolve(source),
    (derived) => track(calls, global, derived),
  );
  // The intrinsic then() does not mark the promise observed, and its rejection
  // handler is what keeps an unhandled call away from the host process. The
  // script may still attach a handler later, so observation is checked again
  // when the run settles.
  const settled: Promise<unknown> = Promise.prototype.then.call(
    promise,
    () => {
      calls.pending.delete(settled);
    },
    (error: unknown) => {
      calls.pending.delete(settled);
      if (!observedPromises.has(promise)) calls.failures.push({ global, error, promise });
    },
  );
  calls.pending.add(settled);
  return promise;
}

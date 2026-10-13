/** Lossless JSON copies for checkpoint payloads and responses that must survive persistence exactly. */

/**
 * Copy a checkpoint payload or response that must survive persistence exactly:
 * the copy equals what a JSON round trip of the value yields. Only plain
 * objects, dense arrays, strings, finite numbers, booleans and null are
 * accepted; -0 becomes 0 because JSON has a single zero; anything else a round
 * trip would change or drop throws a TypeError.
 */
export function cloneDurableJsonValue(value: unknown, label: string): unknown {
  const active = new WeakSet<object>();
  const invalid = (cause?: unknown): never => {
    throw new TypeError(`${label} must be a lossless JSON value`, cause === undefined ? undefined : { cause });
  };
  const clone = (item: unknown, depth: number): unknown => {
    if (item === null || typeof item === "string" || typeof item === "boolean") return item;
    if (typeof item === "number") {
      if (!Number.isFinite(item)) return invalid();
      // -0 === 0, so this returns +0 for both zeros: a persisted payload must
      // still deep-equal the in-memory one, and isDeepStrictEqual tells them apart.
      return item === 0 ? 0 : item;
    }
    if (typeof item !== "object" || depth > 100) return invalid();
    if (active.has(item)) return invalid();
    active.add(item);
    try {
      if (Array.isArray(item)) {
        const ownKeys = Reflect.ownKeys(item);
        if (
          ownKeys.some(
            (key) =>
              typeof key !== "string" ||
              (key !== "length" && (!/^(?:0|[1-9]\d*)$/u.test(key) || Number(key) >= item.length)),
          )
        ) {
          return invalid();
        }
        const arrayCopy: unknown[] = [];
        for (let index = 0; index < item.length; index++) {
          const descriptor = Object.getOwnPropertyDescriptor(item, String(index));
          if (!descriptor?.enumerable || !Object.hasOwn(descriptor, "value")) return invalid();
          arrayCopy.push(clone(descriptor.value, depth + 1));
        }
        return arrayCopy;
      }

      const prototype = Object.getPrototypeOf(item);
      const objectConstructor =
        prototype === null ? Object : Object.getOwnPropertyDescriptor(prototype, "constructor")?.value;
      if (prototype !== null && (typeof objectConstructor !== "function" || objectConstructor.name !== "Object")) {
        return invalid();
      }
      const copy: Record<string, unknown> = {};
      for (const key of Reflect.ownKeys(item)) {
        if (typeof key !== "string") return invalid();
        const descriptor = Object.getOwnPropertyDescriptor(item, key);
        if (!descriptor?.enumerable || !Object.hasOwn(descriptor, "value")) return invalid();
        Object.defineProperty(copy, key, {
          configurable: true,
          enumerable: true,
          writable: true,
          value: clone(descriptor.value, depth + 1),
        });
      }
      return copy;
    } catch (error) {
      return invalid(error);
    } finally {
      active.delete(item);
    }
  };
  return clone(value, 0);
}

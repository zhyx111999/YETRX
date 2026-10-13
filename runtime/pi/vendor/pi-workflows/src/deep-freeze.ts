/** Recursively freezes a value so that nothing reachable from it can change once it is shared. */

/**
 * Freeze `value` and every object reachable through own enumerable properties,
 * and return `value`. An object that is already frozen is not descended into.
 */
export function deepFreeze<T>(value: T): T {
  if (value !== null && typeof value === "object" && !Object.isFrozen(value)) {
    for (const nested of Object.values(value)) deepFreeze(nested);
    Object.freeze(value);
  }
  return value;
}

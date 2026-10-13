// Bounded wait for an execution that is still winding down after a pause or stop.

// The pause tail is a full-state persist, which can be slow on a synced disk.
// 10s bounds a checkpoint reply or resume without failing on such disks.
const DEFAULT_PAUSED_EXECUTION_SETTLE_TIMEOUT_MS = 10_000;
let pausedExecutionSettleTimeoutMs = DEFAULT_PAUSED_EXECUTION_SETTLE_TIMEOUT_MS;

/** @internal Test hook: shorten the settle wait so tests need not take 10s. */
export function _setPausedExecutionSettleTimeoutForTests(ms: number | undefined): void {
  pausedExecutionSettleTimeoutMs = ms ?? DEFAULT_PAUSED_EXECUTION_SETTLE_TIMEOUT_MS;
}

/** Wait for `execution` to settle; false when it is still running after the timeout. */
export async function waitForPausedExecutionSettlement(execution: Promise<unknown>): Promise<boolean> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  const settled = execution.then(
    () => true,
    () => true,
  );
  const timeout = new Promise<boolean>((resolve) => {
    timer = setTimeout(() => resolve(false), pausedExecutionSettleTimeoutMs);
    timer.unref?.();
  });
  const didSettle = await Promise.race([settled, timeout]);
  if (timer) {
    clearTimeout(timer);
  }
  return didSettle;
}

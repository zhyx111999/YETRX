/** Finds a complete JSONL record in an append-only session file without reading the whole history. */

import { closeSync, fstatSync, openSync, readSync } from "node:fs";

/** Last fully-scanned byte offset per session file. Bounded: one entry per touched
 *  file, LRU-evicted past 128 (delete+set on every hit keeps the ACK path's entries
 *  resident). */
const sessionScanOffsets = new Map<string, number>();
const SESSION_SCAN_OFFSETS_CAP = 128;

/** Scan the SDK's JSONL record without allocating the entire session history. */
export function sessionFileContainsEntry(path: string, entry: object): boolean {
  const needle = Buffer.from(`\n${JSON.stringify(entry)}\n`);
  let fd: number | undefined;
  try {
    fd = openSync(path, "r");
    // The delivery ACK re-checks the same file repeatedly, so resume from the
    // last scanned offset instead of offset 0. The cached offset is one past the
    // last scanned byte and a needle straddling it can start at offset-1, so the
    // tail window must start a full needle length earlier; any narrower and a
    // repeat check for an already-present entry would always fall back to a full
    // head scan. On a tail miss, scan the head once: an entry can precede the
    // cached offset when two deliveries interleave.
    const resumeFrom = Math.max(0, (sessionScanOffsets.get(path) ?? 0) - needle.length);
    const foundInTail = scanRegion(fd, needle, resumeFrom);
    const end = lseekEnd(fd);
    rememberScanOffset(path, end);
    if (foundInTail) return true;
    if (resumeFrom > 0) return scanRegion(fd, needle, 0, resumeFrom + needle.length - 1);
    return false;
  } catch {
    return false;
  } finally {
    if (fd !== undefined) closeSync(fd);
  }
}

function rememberScanOffset(path: string, end: number): void {
  sessionScanOffsets.delete(path);
  sessionScanOffsets.set(path, end);
  if (sessionScanOffsets.size > SESSION_SCAN_OFFSETS_CAP) {
    const oldest = sessionScanOffsets.keys().next().value;
    if (oldest !== undefined) sessionScanOffsets.delete(oldest);
  }
}

function lseekEnd(fd: number): number {
  try {
    return fstatSync(fd).size;
  } catch {
    return 0;
  }
}

function scanRegion(fd: number, needle: Buffer, start: number, end?: number): boolean {
  const chunk = Buffer.allocUnsafe(64 * 1024);
  let position = start;
  let tail: Buffer = Buffer.alloc(0);
  for (;;) {
    const budget = end === undefined ? chunk.length : Math.min(chunk.length, end - position);
    if (budget <= 0) return false;
    const count = readSync(fd, chunk, 0, budget, position);
    if (count === 0) return false;
    position += count;
    const window = Buffer.concat([tail, chunk.subarray(0, count)]);
    if (window.indexOf(needle) !== -1) return true;
    tail = window.subarray(Math.max(0, window.length - needle.length + 1));
  }
}

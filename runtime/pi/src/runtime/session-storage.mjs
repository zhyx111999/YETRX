import { chmodSync, mkdirSync } from "node:fs";
import { dirname } from "node:path";

/**
 * Create the directory a session file lives in (private to the current user).
 * @returns the directory, which SessionManager is told to use as-is
 */
export function prepareSessionDirectory(sessionFile) {
  const dir = dirname(sessionFile);
  mkdirSync(dir, { recursive: true, mode: 0o700 });
  return dir;
}

/**
 * pi creates the session file lazily, with the process umask, on the first user or
 * assistant message. Transcripts hold prompts and tool output, so restrict the file to
 * its owner. Returns false while the file does not exist yet.
 */
export function restrictSessionFile(sessionFile) {
  try {
    chmodSync(sessionFile, 0o600);
    return true;
  } catch (error) {
    if (error?.code === "ENOENT") return false;
    throw error;
  }
}

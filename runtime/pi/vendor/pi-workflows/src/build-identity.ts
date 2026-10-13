// Identifies the compiled code a process loaded, so a rebuilt package on disk can be told apart from it.

import { createHash } from "node:crypto";
import { readdirSync, readFileSync } from "node:fs";
import { dirname, join, relative } from "node:path";
import { fileURLToPath } from "node:url";

const CODE_ROOT = dirname(fileURLToPath(import.meta.url));
const MODULE_FILE = /\.(?:[cm]?js|ts)$/;

/**
 * A content hash of the package's modules as they are on disk now, or undefined
 * when they cannot be read. Content rather than timestamps, so a checkout that
 * rewrites identical files is not mistaken for a rebuild.
 */
export function buildIdentityOnDisk(root: string = CODE_ROOT): string | undefined {
  const hash = createHash("sha256");
  const visit = (dir: string): void => {
    const entries = readdirSync(dir, { withFileTypes: true }).sort((a, b) => (a.name < b.name ? -1 : 1));
    for (const entry of entries) {
      const path = join(dir, entry.name);
      if (entry.isDirectory()) visit(path);
      else if (MODULE_FILE.test(entry.name) && !entry.name.endsWith(".d.ts")) {
        hash.update(relative(root, path)).update("\0").update(readFileSync(path)).update("\0");
      }
    }
  };
  try {
    visit(root);
  } catch {
    return undefined;
  }
  return hash.digest("hex").slice(0, 16);
}

/**
 * The build these modules were loaded from. Pi loads the compiled modules
 * with Node's import, which keeps them for the life of the process: /reload
 * re-runs the extension with this code, and a rebuilt package takes effect
 * only after Pi restarts.
 */
export const LOADED_BUILD = buildIdentityOnDisk() ?? "unknown";

/** Whether the package on disk differs from the build this process is running. */
export function loadedBuildIsStale(root?: string): boolean {
  const onDisk = buildIdentityOnDisk(root);
  return onDisk !== undefined && LOADED_BUILD !== "unknown" && onDisk !== LOADED_BUILD;
}

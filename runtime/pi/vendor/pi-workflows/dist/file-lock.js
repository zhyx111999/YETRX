// Cross-process lock files: an owner publishes its complete record in one step, and only one process may reclaim a dead owner's lock.
import { createHash, randomUUID } from "node:crypto";
import { unlinkIfExistsSafe } from "./fs-persistence.js";
/** Whether a process with this id is running. A process this user may not signal still counts as running. */
export function pidIsAlive(pid) {
    if (!Number.isInteger(pid) || pid <= 0)
        return false;
    try {
        process.kill(pid, 0);
        return true;
    }
    catch (err) {
        return err.code === "EPERM";
    }
}
/** The owner a lock file names, or null when it is missing or not an owner record. */
function readFileLock(fs, path) {
    try {
        const owner = JSON.parse(fs.readFileSync(path, "utf-8"));
        if (typeof owner?.token !== "string" || !Number.isInteger(owner.pid) || owner.pid <= 0)
            return null;
        return owner;
    }
    catch {
        return null;
    }
}
/** A fresh token for one claim. */
export function newLockToken() {
    return `${process.pid}-${randomUUID()}`;
}
/**
 * Take the lock at `path` for `owner`. The owner record is written to a private
 * file first and then hard-linked into place, so the lock never exists without
 * its complete content: a reader cannot mistake a half-written lock for a
 * stale one. A lock whose owner `isStale` deems gone is removed and the claim
 * tried once more. Removal is guarded by a second lock keyed by that exact
 * owner's token, so two processes that both saw the same stale owner cannot
 * have the slower one delete the lock the faster one just took.
 *
 * Returns false while another owner holds the lock. A lock file that is not an
 * owner record (outside damage, since owners publish whole files) is treated as
 * held: it is never deleted on a guess.
 */
export function tryClaimFileLock(fs, path, owner, isStale) {
    return claim(fs, path, owner, isStale, 0);
}
function claim(fs, path, owner, isStale, depth) {
    if (depth > 8)
        throw new Error(`Lock recovery chain is too deep at ${path}`);
    const candidate = `${path}.${randomUUID()}.owner`;
    fs.writeFileSync(candidate, JSON.stringify(owner), { flag: "wx" });
    try {
        for (let attempt = 0; attempt < 2; attempt++) {
            try {
                fs.linkSync(candidate, path);
                return true;
            }
            catch (error) {
                if (error.code !== "EEXIST")
                    throw error;
            }
            const existing = readFileLock(fs, path);
            if (!existing || !isStale(existing) || attempt > 0)
                return false;
            const guardKey = createHash("sha256").update(`${path}\0${existing.token}`).digest("hex");
            const guard = `${path}.reap-${guardKey}`;
            if (!claim(fs, guard, owner, isStale, depth + 1))
                return false;
            try {
                if (readFileLock(fs, path)?.token === existing.token)
                    fs.unlinkSync(path);
            }
            finally {
                releaseFileLock(fs, guard, owner.token);
            }
        }
        return false;
    }
    finally {
        unlinkIfExistsSafe(fs, candidate);
    }
}
/** Remove the lock only while `token` still owns it. */
export function releaseFileLock(fs, path, token) {
    if (readFileLock(fs, path)?.token === token)
        unlinkIfExistsSafe(fs, path);
}
/** How long an unreadable claim candidate may be mid-write before it counts as debris. */
const UNREADABLE_DEBRIS_AGE_MS = 60_000;
/**
 * Whether a leftover claim candidate or reap guard can be removed: its owner
 * process is gone. An unreadable one may still be in the middle of its write,
 * so it is debris only once it is old.
 */
export function isDeadLockDebris(fs, file, nowMs = Date.now()) {
    const owner = readFileLock(fs, file);
    if (owner)
        return !pidIsAlive(owner.pid);
    try {
        return nowMs - fs.statSync(file).mtimeMs > UNREADABLE_DEBRIS_AGE_MS;
    }
    catch {
        return false;
    }
}

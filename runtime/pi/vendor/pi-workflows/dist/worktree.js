/**
 * Per-agent git worktree isolation. When an agent requests `isolation: "worktree"`,
 * it runs in a git worktree on its own branch so parallel agents can edit the
 * same files without conflict. Results are NOT auto-merged. The path is logged
 * and kept by default (`keepWorktree: false` deletes after the call).
 */
import { execFile, spawn } from "node:child_process";
import { randomBytes } from "node:crypto";
import { mkdir, rm, rmdir } from "node:fs/promises";
import { join } from "node:path";
import { setTimeout as delay } from "node:timers/promises";
/**
 * Bound every git invocation: a hung git (network FS, credential
 * helper prompt) must not block agent spawn or teardown forever. 30s is far
 * above any local worktree add/remove.
 */
const GIT_TIMEOUT_MS = 30_000;
/** Cleanup after a failed add must not stall the spawn path. */
const CLEANUP_TIMEOUT_MS = 5_000;
/** Keep at most this much of each git output stream (caps a noisy stderr). */
const GIT_OUTPUT_LIMIT = 1024 * 1024;
/**
 * Time a timed-out git's process group gets to exit on SIGTERM before SIGKILL:
 * long enough for git to drop its lock files, short enough not to stall spawn.
 */
const KILL_GRACE_MS = 2_000;
/** How often the grace period checks whether the process group has exited. */
const GROUP_POLL_MS = 50;
const nodeProcessHost = {
    platform: process.platform,
    spawn: (command, args, options) => spawn(command, args, options),
    kill: (pid, signal) => {
        process.kill(pid, signal);
    },
    execFile: (file, args, options, done) => {
        execFile(file, args, options, () => done());
    },
};
const gitExec = (opts) => ({
    timeoutMs: opts?.timeoutMs ?? GIT_TIMEOUT_MS,
    killGraceMs: opts?.killGraceMs ?? KILL_GRACE_MS,
    host: opts?.host ?? nodeProcessHost,
});
class GitTimeoutError extends Error {
}
/**
 * Run git and resolve with its stdout. A timeout ends git's whole process
 * tree, then rejects without waiting for git to exit: a git stuck in the
 * kernel (hung network filesystem) outlives even SIGKILL until its I/O
 * returns, and its hooks keep the new worktree busy unless they end too.
 */
function git(args, exec) {
    return new Promise((resolve, reject) => {
        // On POSIX git leads its own process group, so one signal reaches its hooks too.
        const child = exec.host.spawn("git", args, {
            detached: exec.host.platform !== "win32",
            stdio: ["ignore", "pipe", "pipe"],
            windowsHide: true,
        });
        let stdout = "";
        let stderr = "";
        child.stdout?.setEncoding("utf8").on("data", (chunk) => {
            if (stdout.length < GIT_OUTPUT_LIMIT)
                stdout += chunk;
        });
        child.stderr?.setEncoding("utf8").on("data", (chunk) => {
            if (stderr.length < GIT_OUTPUT_LIMIT)
                stderr += chunk;
        });
        // The first outcome wins; a timeout claims it before the tree is ended, so
        // the exit that the kill causes is not reported as an ordinary failure.
        let decided = false;
        const decide = (settle) => {
            if (decided)
                return;
            decided = true;
            clearTimeout(timer);
            settle();
        };
        const timer = exec.timeoutMs > 0
            ? setTimeout(() => decide(() => {
                const timedOut = () => reject(new GitTimeoutError(`git ${args.join(" ")} timed out after ${exec.timeoutMs}ms`));
                void endProcessTree(child, exec).then(timedOut, timedOut);
            }), exec.timeoutMs)
            : undefined;
        child.on("error", (error) => decide(() => reject(error)));
        child.on("close", (code, signal) => decide(() => {
            if (code === 0) {
                resolve(stdout);
            }
            else {
                const status = signal ? `signal ${signal}` : `exit code ${code}`;
                reject(new Error(`git ${args.join(" ")} failed (${status}): ${stderr.trim()}`));
            }
        }));
    });
}
/** End git and every process it started; resolves within a bounded time whether or not they exit. */
async function endProcessTree(child, exec) {
    // A descendant that inherited git's output pipes must not keep them open.
    child.stdout?.destroy();
    child.stderr?.destroy();
    const pid = child.pid;
    if (pid === undefined)
        return;
    if (exec.host.platform !== "win32")
        return endProcessGroup(pid, exec);
    // taskkill /T finds the descendants through their parent, so it needs git alive.
    if (child.exitCode !== null || child.signalCode !== null)
        return;
    // TerminateProcess (child.kill) would end git.exe alone; it remains the
    // fallback when taskkill fails. taskkill's own timeout bounds the wait.
    await new Promise((resolve) => {
        const finish = () => {
            child.kill();
            resolve();
        };
        try {
            exec.host.execFile("taskkill", ["/pid", String(pid), "/T", "/F"], { timeout: CLEANUP_TIMEOUT_MS, windowsHide: true }, finish);
        }
        catch {
            finish();
        }
    });
}
/**
 * SIGTERM git's process group, then SIGKILL whatever is left after the grace
 * period. The group is probed rather than git itself: git may exit while a
 * hook that ignores SIGTERM keeps running. SIGKILL is not awaited, because a
 * process blocked in the kernel only dies once its I/O returns.
 */
async function endProcessGroup(pid, exec) {
    const signalGroup = (signal) => {
        try {
            exec.host.kill(-pid, signal);
            return true;
        }
        catch {
            return false; // ESRCH: no process is left in the group.
        }
    };
    if (!signalGroup("SIGTERM"))
        return;
    const deadline = Date.now() + exec.killGraceMs;
    while (Date.now() < deadline) {
        await delay(Math.min(GROUP_POLL_MS, deadline - Date.now()));
        if (!signalGroup(0))
            return;
    }
    signalGroup("SIGKILL");
}
function slug(name) {
    return (name
        .toLowerCase()
        .replace(/[^a-z0-9]+/g, "-")
        .replace(/^-+|-+$/g, "")
        .slice(0, 32) || "agent");
}
/**
 * Create an isolated worktree at `<repoRoot>/.pi/worktrees/<id>` on branch
 * `pi/wf/<label slug>-<id>`. The fresh random id makes every live execution's
 * tree and branch unique, so retained results are never reused or overwritten,
 * and leaves the slug free for the agent's label; the run's log names the tree
 * and branch it kept. Journal identity is independent of this path. Returns a
 * failed Worktree on error.
 */
export async function createWorktree(baseCwd, label, execOptions) {
    const exec = gitExec(execOptions);
    let repoRoot;
    try {
        repoRoot = (await git(["-C", baseCwd, "rev-parse", "--show-toplevel"], exec)).trim();
    }
    catch (error) {
        return { isolated: false, cwd: baseCwd, reason: describeGitFailure(error, "not a git repository") };
    }
    let path;
    let branch;
    try {
        ({ path, branch } = await reserveWorktree(repoRoot, label, exec));
    }
    catch (error) {
        return { isolated: false, cwd: baseCwd, reason: describeGitFailure(error, String(error)) };
    }
    try {
        await git(["-C", repoRoot, "worktree", "add", "-b", branch, path, "HEAD"], exec);
        return { isolated: true, cwd: path, branch, repoRoot };
    }
    catch (error) {
        // A timed-out/failed `worktree add` can leave a created branch and a
        // partially checked-out tree behind: git rolls back its
        // registration but never deletes the `-b` branch or the directory.
        await cleanupFailedWorktreeAdd(repoRoot, path, branch, { ...exec, timeoutMs: CLEANUP_TIMEOUT_MS });
        return { isolated: false, cwd: baseCwd, reason: describeGitFailure(error, String(error)) };
    }
}
/** Random worktree id length in bytes; two hex characters each. */
const WORKTREE_ID_BYTES = 4;
/** Collisions are practically impossible; this only bounds a broken environment. */
const WORKTREE_ID_ATTEMPTS = 8;
/**
 * Claim a fresh worktree path and branch. The id stays short because every
 * checked-out path grows by the worktree location's length, and Git for
 * Windows cannot create paths beyond MAX_PATH unless core.longpaths is set.
 * Creating the empty directory is atomic, so no concurrent execution can
 * claim the same id; skipping an id whose branch survives (its tree deleted by
 * hand) means failure cleanup only ever removes what this execution created.
 */
async function reserveWorktree(repoRoot, label, exec) {
    const parent = join(repoRoot, ".pi", "worktrees");
    await mkdir(parent, { recursive: true });
    for (let attempt = 0; attempt < WORKTREE_ID_ATTEMPTS; attempt++) {
        const id = randomBytes(WORKTREE_ID_BYTES).toString("hex");
        const path = join(parent, id);
        try {
            await mkdir(path);
        }
        catch (error) {
            if (error.code === "EEXIST")
                continue;
            throw error;
        }
        const branch = `pi/wf/${slug(label)}-${id}`;
        let free = false;
        try {
            free = !(await git(["-C", repoRoot, "for-each-ref", "--format=%(refname)", `refs/heads/${branch}`], exec)).trim();
        }
        finally {
            if (!free)
                await rmdir(path);
        }
        if (free)
            return { path, branch };
    }
    throw new Error(`no free worktree id under ${parent}`);
}
/** Honest failure text: a killed (timed-out) git is not "not a git repository". */
function describeGitFailure(error, fallback) {
    if (error instanceof GitTimeoutError)
        return "git timed out (slow or hung filesystem?)";
    return error instanceof Error ? error.message : fallback;
}
/** Best-effort cleanup after a failed `worktree add`: drop the half-created
 * branch and any partial checkout left by a killed git. All steps short-timeout
 * and failure-tolerant — the spawn path must not hang on cleanup. */
async function cleanupFailedWorktreeAdd(repoRoot, path, branch, exec) {
    // Order matters: if the killed `worktree add` had already
    // REGISTERED the worktree, `branch -D` is refused ("used by worktree")
    // until the registration is gone. Deregister first, then delete the branch.
    try {
        // Registered case: removes the tree AND the registration in one step.
        await git(["-C", repoRoot, "worktree", "remove", "--force", path], exec);
    }
    catch {
        // best-effort — unregistered partial tree falls through to rm + targeted retry
    }
    try {
        // git and its hooks have ended by now; the retries only absorb Windows
        // releasing a file handle a moment after its process exits.
        await rm(path, { recursive: true, force: true, maxRetries: 3, retryDelay: 100 });
    }
    catch {
        // best-effort
    }
    try {
        // If manual removal left THIS tree registered, retry its removal now that
        // the path is gone. Do not run `git worktree prune`: that mutates unrelated
        // stale registrations elsewhere in the repository.
        await git(["-C", repoRoot, "worktree", "remove", "--force", path], exec);
    }
    catch {
        // best-effort
    }
    try {
        await git(["-C", repoRoot, "branch", "-D", branch], exec);
    }
    catch {
        // best-effort
    }
}
/** Remove a worktree and its branch. Best-effort; safe to call on a no-op Worktree. */
export async function removeWorktree(wt, execOptions) {
    if (!wt.isolated || !wt.repoRoot)
        return;
    const exec = gitExec(execOptions);
    try {
        await git(["-C", wt.repoRoot, "worktree", "remove", "--force", wt.cwd], exec);
    }
    catch {
        // A failed removal (e.g. a locked tree) does not authorize deleting its branch.
        return;
    }
    if (wt.branch) {
        try {
            await git(["-C", wt.repoRoot, "branch", "-D", wt.branch], exec);
        }
        catch {
            // branch already deleted
        }
    }
}

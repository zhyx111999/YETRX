/**
 * Workflow logger with file persistence.
 */

import { appendFileSync, existsSync, mkdirSync, statSync, truncateSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { workflowProjectPaths } from "./workflow-paths.js";

export interface WorkflowLogger {
  log(message: string): void;
  error(message: string): void;
  warn(message: string): void;
  getLogs(): string[];
  persist(): string | null;
}

export interface WorkflowLoggerOptions {
  /** Run ID for persistence. */
  runId?: string;
  /** Working directory for file paths. */
  cwd?: string;
  /** Whether to persist logs to disk. */
  persist?: boolean;
  /** Callback for each log entry. */
  onLog?: (message: string) => void;
}

export function createWorkflowLogger(options: WorkflowLoggerOptions = {}): WorkflowLogger {
  const logs: string[] = [];
  const persistLogs = options.persist ?? true;
  const cwd = options.cwd ?? process.cwd();
  const runId = options.runId ?? `run-${Date.now()}`;
  const runsDir = workflowProjectPaths(cwd).runsDir;
  let logFile: string | null = null;
  // One continuous in-memory append cursor. A failed write must not advance it:
  // the next successful append writes every missing entry in original order.
  // A resumed logger starts at zero but only holds its new execution's logs, so
  // it appends without rewriting the earlier execution's file contents.
  let nextUnpersisted = 0;
  // File length that ended with a complete entry before an append failed. A
  // failed append can still leave part of its chunk on disk (ENOSPC mid-write),
  // and a retry written after that fragment would glue onto it.
  let tornAt: number | null = null;

  /** Cut a failed append's fragment off the log; false while it is still there. */
  const cutTornTail = (file: string): boolean => {
    if (tornAt === null) return true;
    try {
      truncateSync(file, tornAt);
    } catch (error) {
      // A missing file has no fragment left to cut.
      if ((error as NodeJS.ErrnoException).code !== "ENOENT") return false;
    }
    tornAt = null;
    return true;
  };

  const flushPendingEntries = () => {
    if (!logFile || logs.length === nextUnpersisted) return;
    if (!cutTornTail(logFile)) throw new Error(`cannot cut a torn entry off ${logFile}`);
    // Measured right before the append instead of summed from earlier appends:
    // a superseded execution that is still settling may append to the same file.
    const boundary = statSync(logFile, { throwIfNoEntry: false })?.size ?? 0;
    try {
      appendFileSync(logFile, `${logs.slice(nextUnpersisted).join("\n")}\n`);
    } catch (error) {
      tornAt = boundary;
      // Cut at once, before another writer appends after the fragment; a cut
      // that fails here is retried before the next append.
      cutTornTail(logFile);
      throw error;
    }
    nextUnpersisted = logs.length;
  };

  const write = (level: string, message: string) => {
    const timestamp = new Date().toISOString();
    const entry = `[${timestamp}] [${level}] ${message}`;
    logs.push(entry);
    options.onLog?.(message);

    if (persistLogs && logFile) {
      try {
        flushPendingEntries();
      } catch {
        // Silent fail for log persistence — a later flush retries in order.
      }
    }
  };

  const logger: WorkflowLogger = {
    log(message: string) {
      write("INFO", message);
    },
    error(message: string) {
      write("ERROR", message);
    },
    warn(message: string) {
      write("WARN", message);
    },
    getLogs() {
      return [...logs];
    },
    persist() {
      if (!persistLogs) return null;
      try {
        mkdirSync(runsDir, { recursive: true });
        logFile = join(runsDir, `${runId}.log`);
        flushPendingEntries();
        if (!existsSync(logFile)) {
          writeFileSync(logFile, "");
        }
        return logFile;
      } catch {
        return null;
      }
    },
  };

  // Initialize log file if persisting
  if (persistLogs) {
    try {
      mkdirSync(runsDir, { recursive: true });
      logFile = join(runsDir, `${runId}.log`);
    } catch {
      // Silent fail
    }
  }

  return logger;
}

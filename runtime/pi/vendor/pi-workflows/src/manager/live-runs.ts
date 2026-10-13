// Keeps the manager's in-memory runs in step with their records, ignoring superseded executions.

import type { EventEmitter } from "node:events";
import { RunRecordDeletedError } from "../errors.js";
import { reportDiagnostic } from "../host-diagnostics.js";
import type { RunPersistence } from "../run-persistence.js";
import type { ManagedRun } from "../workflow-manager.js";
import { BURST_PERSIST_MS, PersistThrottle, PROGRESS_PERSIST_MS } from "./persist-throttle.js";
import { runRecord, settleAgentsForTerminalRun } from "./run-record.js";
import { TerminalRunRetention } from "./terminal-retention.js";

/**
 * The write side of the manager's run map. When resume() replaces a run's
 * entry or deleteRun() removes it, the old execution is no longer current: it
 * may still settle later, but must not write the record, touch the lease, or
 * emit events, or it would recreate a deleted run or overwrite a newer one.
 */
export class LiveRuns {
  readonly persistence: RunPersistence;
  private readonly runs: Map<string, ManagedRun>;
  private readonly events: EventEmitter;
  private readonly throttle = new PersistThrottle();
  private readonly retention: TerminalRunRetention<ManagedRun>;
  /** Runs deleted here or by another process; an execution still winding down finishes their deletion. */
  private readonly deleted = new WeakSet<ManagedRun>();

  constructor(
    runs: Map<string, ManagedRun>,
    persistence: RunPersistence,
    events: EventEmitter,
    maxTerminalRunsInMemory: number,
  ) {
    this.runs = runs;
    this.persistence = persistence;
    this.events = events;
    this.retention = new TerminalRunRetention(runs, maxTerminalRunsInMemory);
  }

  get(runId: string): ManagedRun | undefined {
    return this.runs.get(runId);
  }

  /** Whether `managed` is still the entry for its run id. */
  isCurrent(managed: ManagedRun): boolean {
    return this.runs.get(managed.runId) === managed;
  }

  /**
   * Emit an event for an execution that is still current. A superseded one's
   * "complete" would otherwise deliver a result for a run that is gone or
   * replaced. Lifecycle commands emit directly: they are current when they fire.
   */
  emit(managed: ManagedRun, event: string, payload: unknown): void {
    if (this.isCurrent(managed)) this.events.emit(event, payload);
  }

  hasListeners(event: string): boolean {
    return this.events.listenerCount(event) > 0;
  }

  /**
   * Write the run now, replacing any pending throttled write, so the newest
   * state lands last. Every lifecycle change writes through here.
   */
  persistRun(managed: ManagedRun, required = false): void {
    if (!this.isCurrent(managed)) return;
    this.throttle.cancel(managed.runId);
    this.writeRunToDisk(managed, required);
  }

  /** Write the run after a short window, for writes that come in bursts. */
  schedulePersist(managed: ManagedRun): void {
    this.throttle.schedule(managed.runId, () => this.writeRunToDisk(managed), BURST_PERSIST_MS);
  }

  /** Write the run after the long progress window, for running agents' streaming usage and history. */
  scheduleProgressPersist(managed: ManagedRun): void {
    this.throttle.schedule(managed.runId, () => this.writeRunToDisk(managed), PROGRESS_PERSIST_MS);
  }

  /**
   * The single path to disk. The current-entry check repeats persistRun()'s
   * because the throttled write arrives here directly. Progress writes are
   * best-effort; `required` writes (checkpoints) throw. The save keeps the
   * record's session owner, which another process may have moved, and the
   * run takes it over.
   */
  private writeRunToDisk(managed: ManagedRun, required = false): void {
    if (!this.isCurrent(managed)) return;
    try {
      const now = new Date();
      settleAgentsForTerminalRun(managed, now);
      const record = runRecord(managed, now);
      this.persistence.save(record);
      managed.sessionId = record.sessionId;
    } catch (err) {
      // Deletion by another process is authoritative: the local copy is stale.
      const deleted = err instanceof RunRecordDeletedError;
      if (deleted) this.forget(managed);
      if (required) throw err;
      if (!deleted) reportDiagnostic(`Could not save workflow run ${managed.runId}`, err);
    }
  }

  releaseLease(managed: ManagedRun): void {
    if (!managed.lease) return;
    this.persistence.releaseRunLease(managed.lease);
    managed.lease = undefined;
  }

  /** A run whose execution settled to a terminal status may now be evicted from memory. */
  recordTerminalRun(managed: ManagedRun): void {
    this.retention.record(managed.runId, managed);
  }

  /** Remove a run being deleted: its entry and any pending write go; its execution, if any, finishes the deletion. */
  remove(runId: string, managed: ManagedRun | undefined): void {
    this.runs.delete(runId);
    if (managed) this.deleted.add(managed);
    this.throttle.cancel(runId);
  }

  /**
   * After an execution ends, delete a deleted run's files again: its script
   * may have caught the abort and kept writing its run log.
   */
  deleteLeftovers(managed: ManagedRun): void {
    if (!this.deleted.has(managed)) return;
    try {
      this.persistence.delete(managed.runId);
    } catch {
      // Best-effort: a leftover log never resurrects the run.
    }
  }

  /**
   * Another process deleted this run's record. Mirror a local deleteRun(): stop
   * any live execution, drop the in-memory copy, and notify watchers, so no
   * later write can recreate the run.
   */
  forget(managed: ManagedRun): void {
    if (!this.isCurrent(managed)) return;
    if (!managed.controller.signal.aborted) managed.controller.abort();
    this.remove(managed.runId, managed);
    this.releaseLease(managed);
    this.events.emit("deleted", { runId: managed.runId });
  }

  /**
   * Drop a settled local copy whose record another process deleted. A copy
   * still executing holds the lease, and its next write detects the deletion;
   * a record that exists but fails validation is damaged, not deleted.
   */
  dropIfDeleted(cached: ManagedRun | undefined): void {
    if (!cached || cached.lease) return;
    try {
      if (this.persistence.loadPreview(cached.runId)) return;
    } catch {
      return;
    }
    this.forget(cached);
  }
}

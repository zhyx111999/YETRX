// Rejects working directories that Windows would silently replace with a different directory when it starts a process.
import { WorkflowError, WorkflowErrorCode } from "../errors.js";

/**
 * fs calls reach such a directory literally, but process creation normalizes
 * the path: it drops a single trailing period from inner segments and all
 * trailing periods and spaces from the last segment. The agent's shell would
 * then run in a different, possibly existing, directory than its file tools.
 */
export function assertProcessesStartIn(cwd: string, label: string | undefined): void {
  if (process.platform !== "win32") return;
  const segments = cwd.split("\\");
  const startsIn = segments
    .map((segment, index) =>
      index === segments.length - 1 ? segment.replace(/[. ]+$/, "") : segment.replace(/([^.])\.$/, "$1"),
    )
    .join("\\");
  if (startsIn === cwd) return;
  throw new WorkflowError(
    `agent cwd "${cwd}" cannot be used on Windows: processes started there run in "${startsIn}", because Windows drops trailing dots and spaces from path segments`,
    WorkflowErrorCode.SCRIPT_VALIDATION_ERROR,
    { recoverable: false, agentLabel: label },
  );
}

import { errorText, log, redact } from "../diagnostics.mjs";

/**
 * Writes protocol records: one JSON object per line, LF-terminated, in call order.
 * JSON.stringify never emits a raw LF, so a record can never be split by a reader
 * that frames on LF.
 *
 * @param {(chunk: string, callback?: (error?: Error | null) => void) => boolean} write raw fd-1 writer
 */
export function createRecordWriter(write) {
  return {
    send(record) {
      let line;
      try {
        line = JSON.stringify(record);
      } catch (error) {
        log(`dropped unserializable ${record?.type ?? "record"}: ${errorText(error)}`);
        return;
      }
      write(`${redact(line)}\n`);
    },
    /** Resolves once everything sent so far has been handed to the OS. */
    flush() {
      return new Promise((resolve) => write("", () => resolve()));
    },
  };
}

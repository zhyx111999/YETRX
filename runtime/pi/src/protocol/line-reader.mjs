import { StringDecoder } from "node:string_decoder";

/**
 * Splits a byte stream into records on LF only (an optional preceding CR is
 * stripped). Node's `readline` is unsuitable: it also splits on U+2028/U+2029,
 * which are legal inside JSON strings.
 *
 * @param {(line: string) => void} onLine called once per non-empty record
 */
export function createLineReader(onLine) {
  const decoder = new StringDecoder("utf8");
  let buffered = "";

  const emit = (raw) => {
    const line = raw.endsWith("\r") ? raw.slice(0, -1) : raw;
    if (line.length > 0) onLine(line);
  };

  return {
    /** @param {Buffer | string} chunk */
    push(chunk) {
      const scanFrom = buffered.length;
      buffered += typeof chunk === "string" ? chunk : decoder.write(chunk);
      let start = 0;
      for (let end = buffered.indexOf("\n", scanFrom); end !== -1; end = buffered.indexOf("\n", start)) {
        emit(buffered.slice(start, end));
        start = end + 1;
      }
      if (start > 0) buffered = buffered.slice(start);
    },
    /** Flush a final record that has no trailing LF. */
    end() {
      buffered += decoder.end();
      const rest = buffered;
      buffered = "";
      emit(rest);
    },
  };
}

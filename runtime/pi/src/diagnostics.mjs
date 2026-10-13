/**
 * Process-wide diagnostics: stdout belongs to the protocol, everything else goes
 * to stderr, and registered secrets never leave the process in either place.
 */

const secrets = new Set();

/** Remember a secret so it is scrubbed from stderr and protocol output. */
export function registerSecret(value) {
  if (typeof value === "string" && value.length >= 8) secrets.add(value);
}

/** Replace every registered secret in `text`. */
export function redact(text) {
  let out = text;
  for (const secret of secrets) out = out.replaceAll(secret, "[redacted]");
  return out;
}

/**
 * Take exclusive ownership of fd 1. Returns the raw writer for protocol records;
 * anything else that writes to `process.stdout` (a stray console.log in an
 * extension, say) is redirected to stderr so it can never corrupt the framing.
 *
 * @returns {(chunk: string, callback?: (error?: Error | null) => void) => boolean}
 */
export function claimProtocolStdout() {
  const rawWrite = process.stdout.write.bind(process.stdout);
  const rawErrWrite = process.stderr.write.bind(process.stderr);
  const toStderr = (chunk, ...rest) => rawErrWrite(typeof chunk === "string" ? redact(chunk) : chunk, ...rest);
  process.stdout.write = toStderr;
  process.stderr.write = toStderr;
  return (chunk, callback) => rawWrite(chunk, "utf8", callback);
}

/** One diagnostic line on stderr. */
export function log(message) {
  process.stderr.write(`[yetrx-pi] ${message}\n`);
}

/** Message text of anything thrown, with secrets removed. */
export function errorText(error) {
  const text = error instanceof Error ? error.message : String(error);
  return redact(text);
}

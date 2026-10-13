# Focused recipes

Read only the recipe matching the concern. Change task prompts, schemas, bounds, and context-supplied inputs; preserve the listed contract.

| Concern | Preserve when adapting | Tested recipe |
| --- | --- | --- |
| Semantic retry | Separate it from recoverable runtime retries. Use a new unique label per bounded attempt and return the attempt ledger plus exhausted outcome. | [Bounded semantic retry](../examples/bounded-semantic-retry.js) |
| Structured fields | Pass a small plain JSON Schema before reading fields. Ledger recoverable `null`; treat exhausted schema repair as a nonrecoverable failure. | [Structured output](../examples/structured-output.js) |
| Defensive text parsing | Without `schema`, `agent()` always resolves to raw text, even when the prompt asks for JSON. Parse and validate shape before reading fields; ledger unparseable text separately from recoverable `null`. Prefer `schema` when the shape matters. | [Defensive JSON parsing](../examples/defensive-json-parsing.js) |

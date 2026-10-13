# Workflow review checklist

Review author-visible behavior, not formatting preferences.

## Envelope and contract

- Is literal `export const meta` the first statement, with a short unique name and useful description?
- Are only used phases declared, and does each named phase begin at the intended boundary?
- Does the script explicitly return JSON-serializable data?
- Are imports and nondeterministic APIs absent?

## Topology and identity

- Does topology match dependencies: thunks for independent parallel work, stages for per-item pipelines, barriers before whole-set synthesis?
- Is cardinality bounded before fan-out?
- Does each subagent do one task, with no follow-up subagent that only cross-checks another one?
- Is every agent label short and unique?
- Are stable work-unit IDs retained beside ordered results?
- Are failed/null identities recorded before any filtering?

## Data and routing

- Does JavaScript consume structured fields only after a small plain JSON Schema guarantees them?
- Does synthesis receive complete coverage and failure ledgers?
- Are `model`, `tier`, and `agentType` used according to selector priority?
- Did every nonstandard route or agent type come from context with a name and purpose?

## Lifecycle

- Are semantic retries bounded in the script, separately from the runtime retries set in settings?
- Are loops and fan-out bounded appropriately for the configured agent cap?
- Does every checkpoint use a stable, unique `checkpointId` and a JSON payload?
- Would the script body and each fan-out branch make their calls in the same order on resume?

## Compatibility and publication

- Does new code use `log()` rather than compatibility-only `console`?
- Is compatibility behavior clearly distinguished from supported authoring behavior and VM substrate?
- Do package, skill, and generated contract versions match?
- Do all relative links resolve within the publishable package?

Use [lifecycle](lifecycle.md) for lifecycle reasoning. Open the compact [capability index](capabilities.md) only when the review turns on a disputed signature, default, support boundary, or installed version; follow its exhaustive-facts pointer only when the index is insufficient.

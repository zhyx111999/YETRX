export const meta = {
  name: "bounded_semantic_retry",
  description: "Keep a visible bounded semantic attempt ledger, separate from runtime retries",
  phases: [{ title: "Attempt" }],
};

// ADAPT: define task-owned acceptance, prompts, bounds, and structured result fields.
const maxSemanticAttempts =
  args && Number.isInteger(args.maxSemanticAttempts) ? Math.max(1, Math.min(args.maxSemanticAttempts, 5)) : 3;
const resultSchema = {
  type: "object",
  properties: {
    accepted: { type: "boolean" },
    answer: { type: "string" },
    feedback: { type: "string" },
  },
  required: ["accepted", "answer", "feedback"],
};
const attempts = [];
let feedback = "";
let acceptedResult = null;

phase("Attempt");
for (let attempt = 1; attempt <= maxSemanticAttempts; attempt++) {
  // Runtime retries (the defaultAgentRetries setting) repeat one logical call
  // after recoverable execution failures; each iteration here is a new call.
  const result = await agent(
    `Produce an acceptable answer.${feedback ? ` Address this prior feedback: ${feedback}` : ""}`,
    { label: `semantic-attempt:${attempt}`, schema: resultSchema },
  );
  if (result === null) {
    attempts.push({ attempt, status: "missing", result: null });
    feedback = "The previous logical attempt produced no usable coverage.";
    continue;
  }
  const status = result.accepted ? "accepted" : "rejected";
  attempts.push({ attempt, status, result });
  if (result.accepted) {
    acceptedResult = result;
    break;
  }
  feedback = result.feedback;
}

// INVARIANT: semantic exhaustion is returned visibly; it is not presented as success or thrown away.
return {
  ok: acceptedResult !== null,
  exhausted: acceptedResult === null && attempts.length === maxSemanticAttempts,
  result: acceptedResult,
  attempts,
};

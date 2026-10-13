# Checkpoints

`checkpoint({ kind, checkpointId, payload })` durably suspends the run until a controller attaches a JSON response for that exact ID and resumes it; replay returns the journaled response. It takes exactly these three fields; a prompt string is rejected.

Use a stable, unique `checkpointId` and a lossless JSON payload. The pause is not announced in the conversation: `subagents_control` `status` shows the waiting checkpoint's ID, kind, and payload, and `resume` with the `runId`, that `checkpointId`, and a JSON `response` answers it. Hosts call `WorkflowManager.attachCheckpointResponse(runId, checkpointId, response)` before `resume(runId, { checkpointId })`; attachment alone does not restart execution. The response survives process restart. Do not catch checkpoint suspension to continue work: it remains a run-level pause. This form does not display a confirmation dialog or grant permission to perform external actions.

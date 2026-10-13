/**
 * Deep research workflow.
 * Built-in workflow for comprehensive research across multiple sources.
 */

import { MAX_CONCURRENCY } from "./config.js";

/** Search queries planned when `args.angles` is omitted. */
export const DEEP_RESEARCH_DEFAULT_ANGLES = 4;

/**
 * Upper bound for `args.angles`. Each angle is one research subagent that
 * runs a web search and two fetches, so cost grows linearly with it, while one
 * planning call yields few genuinely distinct queries beyond a dozen or so.
 * Capping at the concurrency ceiling keeps every query in a single wave.
 */
export const DEEP_RESEARCH_MAX_ANGLES = MAX_CONCURRENCY;

/**
 * Generate a deep-research workflow that uses the real web_search/web_fetch tools.
 *
 * The script is static and reads its inputs from `args` (question/angles),
 * so the question is never string-interpolated into source — no escaping hazards.
 * builtin-workflows.ts validates `args` before a run starts, so the script trusts
 * them. The run supplies the web tools (see builtin-workflows.ts). The result keeps
 * every query with its outcome and sources, and says how the queries were
 * planned, so the parent model can check coverage as well as the claims.
 */
export function generateDeepResearchWorkflow(): string {
  return `export const meta = {
  name: 'deep_research',
  description: 'Deep research: plan queries, gather sourced claims, return them to the parent',
  phases: [
    { title: 'Queries' },
    { title: 'Gather' },
  ],
}

const question = args.question
const angles = args.angles ?? ${DEEP_RESEARCH_DEFAULT_ANGLES}

phase('Queries')
const plan = await agent(
  'You are planning web research for this question:\\n' + question +
  '\\n\\nProduce ' + angles + ' diverse, specific search queries that together cover the question from different angles.',
  { label: 'plan queries', schema: { type: 'object', properties: { queries: { type: 'array', items: { type: 'string' } } }, required: ['queries'] } }
)
// The planner agent() can return null (e.g. a subagent that died on a terminal
// provider error) or omit a usable queries array. Fall back to the original
// question as a single query so research still proceeds (degraded) instead of
// crashing on plan.queries.
const planned = plan && Array.isArray(plan.queries) ? plan.queries.filter((q) => typeof q === 'string' && q.trim().length > 0) : []
const fellBack = planned.length === 0
const queries = (fellBack ? [question] : planned).slice(0, angles)
// How the queries came about: fewer than requested, or the question itself when
// planning failed, is reduced coverage the parent should know about.
const planning = { requestedAngles: angles, plannedQueries: queries.length, fellBackToQuestion: fellBack }

phase('Gather')
const gathered = await parallel(queries.map((q, i) => () =>
  agent(
    'Research this query using the web_search and web_fetch tools.\\nQuery: ' + q +
    '\\n\\nSteps: (1) call web_search with the query; (2) web_fetch the 2 most relevant result URLs; ' +
    '(3) extract concrete, verifiable factual claims, each tagged with the exact source URL it came from. ' +
    'Do NOT invent sources or claims — report only what the fetched pages actually say.',
    { label: 'research ' + (i + 1), schema: { type: 'object', properties: { sources: { type: 'array', items: { type: 'object', properties: { url: { type: 'string' }, claims: { type: 'array', items: { type: 'string' } } }, required: ['url', 'claims'] } } }, required: ['sources'] } }
  )
))
// parallel() turns a failed research agent into null. Keep each query's outcome
// next to its sources so the parent can tell a query that found nothing (ok with
// no sources) from one whose research never finished (failed).
const results = queries.map((query, i) => {
  const found = gathered[i]
  return found && Array.isArray(found.sources)
    ? { query, status: 'ok', sources: found.sources }
    : { query, status: 'failed' }
})

return { question, planning, results }`;
}

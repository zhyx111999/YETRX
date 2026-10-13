/** Groups a run's agents by phase: the one phase list every progress surface lays out. */
import { asText } from "./navigator/as-text.js";
/** How a phase is printed: a blank name and the phase-less group get readable stand-ins. */
export function phaseTitle(phase) {
    if (phase === null)
        return "(no phase)";
    return phase.trim() ? phase : "(unnamed phase)";
}
/** An agent's group: its phase as text, or null when it ran outside any phase. */
function phaseOf(agent) {
    return agent.phase == null ? null : asText(agent.phase);
}
/**
 * Declared phases come first, in declared order, including those no agent has
 * reached yet. Every phase that only an agent names (an explicit
 * `agent({ phase })`, or the phase-less group) follows in the order its first
 * agent appears. Snapshots can come from corrupt records or unchecked scripts,
 * so non-array fields read as empty and phase names are coerced to text.
 */
export function groupAgentsByPhase(snapshot) {
    const groups = new Map();
    const groupOf = (phase) => {
        let group = groups.get(phase);
        if (!group) {
            group = {
                phase,
                title: phaseTitle(phase),
                agents: [],
                counts: { queued: 0, running: 0, done: 0, error: 0, skipped: 0 },
            };
            groups.set(phase, group);
        }
        return group;
    };
    for (const name of Array.isArray(snapshot.phases) ? snapshot.phases : [])
        groupOf(asText(name));
    for (const agent of Array.isArray(snapshot.agents) ? snapshot.agents : []) {
        const group = groupOf(phaseOf(agent));
        group.agents.push(agent);
        if (Object.hasOwn(group.counts, agent.status))
            group.counts[agent.status]++;
    }
    return [...groups.values()];
}

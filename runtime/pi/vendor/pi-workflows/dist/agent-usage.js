/** Create an independent zero-valued agent usage record. */
export function createEmptyAgentUsage() {
    return { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0, cost: 0 };
}
/** Add agent usage records without mutating either input. */
export function sumAgentUsage(...records) {
    const total = createEmptyAgentUsage();
    for (const usage of records) {
        total.input += usage.input;
        total.output += usage.output;
        total.cacheRead += usage.cacheRead;
        total.cacheWrite += usage.cacheWrite;
        total.total += usage.total;
        total.cost += usage.cost;
        if (usage.estimated)
            total.estimated = true;
    }
    return total;
}
/**
 * Track provisional and committed usage for one logical agent call across retries.
 * Starting a new attempt closes older attempts so their late callbacks are ignored.
 */
export function createAgentCallUsageTracker(onUpdate) {
    let committedCallUsage = createEmptyAgentUsage();
    let activeAttempt = 0;
    return {
        startAttempt() {
            const attemptId = ++activeAttempt;
            let attemptUsage = createEmptyAgentUsage();
            let terminalUsage;
            let closed = false;
            const isOpen = () => !closed && attemptId === activeAttempt;
            const emitProgress = () => {
                onUpdate({ tokenUsage: sumAgentUsage(committedCallUsage, attemptUsage) });
            };
            const commitUsage = (usage) => {
                if (!isOpen()) {
                    return { tokens: 0 };
                }
                closed = true;
                committedCallUsage = sumAgentUsage(committedCallUsage, usage);
                onUpdate({ tokenUsage: committedCallUsage, committedUsage: usage });
                return { tokens: committedCallUsage.total, tokenUsage: committedCallUsage };
            };
            return {
                reportProgress(usage) {
                    if (!isOpen() || agentUsageEquals(attemptUsage, usage)) {
                        return;
                    }
                    attemptUsage = usage;
                    emitProgress();
                },
                reportTerminal(usage) {
                    if (!isOpen()) {
                        return;
                    }
                    terminalUsage = usage;
                    if (!agentUsageEquals(attemptUsage, usage)) {
                        attemptUsage = usage;
                        emitProgress();
                    }
                },
                commitWithFallback(fallbackTotal) {
                    if (!isOpen())
                        return { tokens: 0 };
                    if (terminalUsage && (terminalUsage.total > 0 || terminalUsage.cost > 0)) {
                        return commitUsage(terminalUsage);
                    }
                    // Lazy: the fallback estimate JSON.stringifies the full result+prompt
                    // — only pay that when no nonzero terminal tokens/cost were reported.
                    // A missing provider usage report can surface as all-zero SDK stats.
                    // Keep the heuristic explicitly tagged throughout persistence/display.
                    return commitUsage({ ...createEmptyAgentUsage(), total: Math.max(0, fallbackTotal()), estimated: true });
                },
                commitTerminalUsage() {
                    if (!terminalUsage) {
                        if (!isOpen()) {
                            return { tokens: 0 };
                        }
                        closed = true;
                        const displayedUsage = sumAgentUsage(committedCallUsage, attemptUsage);
                        if (!agentUsageEquals(displayedUsage, committedCallUsage)) {
                            onUpdate({ tokenUsage: committedCallUsage });
                        }
                        return { tokens: 0 };
                    }
                    return commitUsage(terminalUsage);
                },
            };
        },
    };
}
/** Return whether two complete agent usage records contain the same values. */
export function agentUsageEquals(left, right) {
    return (left.input === right.input &&
        left.output === right.output &&
        left.cacheRead === right.cacheRead &&
        left.cacheWrite === right.cacheWrite &&
        left.total === right.total &&
        left.cost === right.cost &&
        // The flag is part of the value: replacing an estimate with exact figures
        // (same numbers) must still emit an update so the stale flag clears.
        !left.estimated === !right.estimated);
}

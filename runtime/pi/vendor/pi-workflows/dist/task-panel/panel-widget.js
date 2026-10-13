/** The live panel component: redraws on run events, and on a timer in detailed mode while a run is active. */
import { clampMaxAgents, clearTokenSamples, renderPanel, renderPanelDetailed } from "./panel-render.js";
// `tokenUsage` keeps the detailed panel's token/s counter fresh as tokens accrue,
// not only on agent start/end; in compact mode it just redraws identical content.
const RUN_EVENTS = [
    "agentStart",
    "agentModel",
    "agentEnd",
    "phase",
    "log",
    "tokenUsage",
    "complete",
    "error",
    "stopped",
    "paused",
    "resumed",
];
/** Events after which a run is gone and its token-rate samples can be dropped. */
const RUN_END_EVENTS = ["complete", "error", "stopped"];
/**
 * Re-read settings at most once a second: often enough that /workflows-progress
 * applies without a restart, without a disk read on every frame. Without a
 * loader the panel stays compact.
 */
function throttledSettings(loadSettings) {
    let cached = {};
    let cachedAt = Number.NEGATIVE_INFINITY;
    return () => {
        if (!loadSettings)
            return cached;
        const now = Date.now();
        if (now - cachedAt > 1000) {
            try {
                cached = loadSettings() ?? {};
            }
            catch {
                cached = {};
            }
            cachedAt = now;
        }
        return cached;
    };
}
/** Widget factory for the panel. It takes no input; the user opens the navigator with /workflows. */
export function panelWidget(manager, loadSettings) {
    const settings = throttledSettings(loadSettings);
    const hasActiveRun = () => manager.listRuns().some((r) => r.status === "running" || r.status === "paused");
    return (tui, theme) => {
        const onEvent = () => tui.requestRender();
        for (const ev of RUN_EVENTS)
            manager.on(ev, onEvent);
        const onRunEnd = ({ runId }) => clearTokenSamples(runId);
        for (const ev of RUN_END_EVENTS)
            manager.on(ev, onRunEnd);
        // In detailed mode, redraw every 2s while a run is active so the token/s
        // rate keeps updating between sparse token events and decays to 0 when an
        // agent stalls. Check for active runs first: settings() is a synchronous
        // disk read and this tick always outlives its cache, so an idle panel
        // would otherwise read the config every tick.
        const timer = setInterval(() => {
            if (hasActiveRun() && settings().progressPanelMode === "detailed")
                tui.requestRender();
        }, 2000);
        timer.unref?.();
        return {
            render: (width) => {
                const s = settings();
                if (s.progressPanelMode === "detailed") {
                    return renderPanelDetailed(manager, theme, width, clampMaxAgents(s.progressPanelMaxAgents), Date.now());
                }
                return renderPanel(manager, theme, width);
            },
            invalidate: () => { },
            dispose: () => {
                clearInterval(timer);
                for (const ev of RUN_EVENTS)
                    manager.off(ev, onEvent);
                for (const ev of RUN_END_EVENTS)
                    manager.off(ev, onRunEnd);
            },
        };
    };
}

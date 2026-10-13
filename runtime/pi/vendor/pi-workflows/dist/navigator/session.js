/** What one open navigator overlay acts on, shared by its input handlers. */
/** The storage in effect now; the extension can replace it after session start. */
export function currentStorage(session) {
    return session.options.getStorage?.();
}
/** The runs-pane items as the user currently sees them, including a filter being typed. */
export function currentSnapshot(session) {
    return session.model.visible(session.state.effectiveFilter());
}

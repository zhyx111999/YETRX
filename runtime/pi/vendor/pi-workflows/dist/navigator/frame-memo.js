/** Memoizes data lookups for the duration of one render frame. */
/**
 * One frame reads the same runs, saved workflows, and snapshots many times
 * (list, header, panes, footer). Inside `run`, each key is computed once so
 * the frame sees one consistent view; outside a frame every read is fresh.
 */
export class FrameMemo {
    depth = 0;
    values = new Map();
    run(render) {
        this.depth++;
        try {
            return render();
        }
        finally {
            if (--this.depth === 0)
                this.values.clear();
        }
    }
    get(key, compute) {
        if (this.depth === 0)
            return compute();
        if (!this.values.has(key))
            this.values.set(key, compute());
        return this.values.get(key);
    }
}

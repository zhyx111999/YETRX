/** A bounded per-overlay cache for expensive Markdown parsing, highlighting, and result stringification. */
const MAX_ENTRIES = 96;
const MAX_WEIGHT = 4_000_000;
export class NavigatorTextRenderCache {
    entries = new Map();
    resultJson = new WeakMap();
    weight = 0;
    get(key) {
        const hit = this.entries.get(key);
        if (!hit)
            return undefined;
        // Refresh insertion order so eviction behaves like a small LRU.
        this.entries.delete(key);
        this.entries.set(key, hit);
        return hit.lines;
    }
    stringify(result) {
        const cached = this.resultJson.get(result);
        if (cached !== undefined)
            return cached;
        let json;
        try {
            json = JSON.stringify(result, null, 2) ?? String(result);
        }
        catch {
            json = String(result);
        }
        this.resultJson.set(result, json);
        return json;
    }
    /** Drop every rendered entry. They embed the active theme's ANSI colors, so a
     * theme change must not reuse them; result JSON is theme-free and stays. */
    clear() {
        this.entries.clear();
        this.weight = 0;
    }
    set(key, lines, weight) {
        if (weight > MAX_WEIGHT)
            return lines;
        const previous = this.entries.get(key);
        if (previous)
            this.weight -= previous.weight;
        this.entries.delete(key);
        this.entries.set(key, { lines, weight });
        this.weight += weight;
        while (this.entries.size > MAX_ENTRIES || this.weight > MAX_WEIGHT) {
            const oldest = this.entries.entries().next().value;
            if (!oldest)
                break;
            this.entries.delete(oldest[0]);
            this.weight -= oldest[1].weight;
        }
        return lines;
    }
}

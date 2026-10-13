/**
 * Numbers agent() and checkpoint() calls by where they sit in the script, so a
 * call keeps its journal index however its concurrent siblings are timed.
 *
 * The script body and each branch of a parallel()/pipeline() fan-out number
 * their own calls in the order they make them. A fan-out takes one position
 * per branch when it starts, and a branch's first call is numbered by its
 * branch's position; the branch's later calls, and the branches of fan-outs it
 * starts, extend that position. Top-level positions are the journal index
 * itself, so a sequential script or a task list numbers its calls 0, 1, 2, …;
 * deeper positions hash into a range above them.
 *
 * When a fan-out settles, its parent moves past one more position per call its
 * branches made. A call a branch no longer makes thus shifts every later call
 * of the parent off its journaled position, just as a removed call shifts the
 * calls after it in a sequential script.
 */
import { createHash } from "node:crypto";
/** Journal indexes of nested positions start here, far above any top-level position. */
const NESTED_INDEX_BASE = 2 ** 52;
/** The journal index of a position: top-level positions as-is, nested ones hashed into [2^52, 2^53). */
function positionIndex(position) {
    if (position.length === 1)
        return position[0];
    const digest = createHash("sha256").update(position.join(".")).digest("hex");
    // 13 hex digits are 52 bits, so the sum stays a safe integer.
    return NESTED_INDEX_BASE + Number.parseInt(digest.slice(0, 13), 16);
}
/** The calls of the script body or of one fan-out branch. */
export class CallSequence {
    position;
    taken;
    next = 0;
    /** Calls made here and in the settled fan-outs started here. */
    calls = 0;
    constructor(
    /** Empty for the script body; a branch's position otherwise. */
    position, 
    /** Indexes taken anywhere in the run, shared by every sequence of it. */
    taken) {
        this.position = position;
        this.taken = taken;
    }
    /** The script body's sequence; every other sequence of the run forks from it. */
    static forScript() {
        return new CallSequence([], new Set());
    }
    /**
     * The journal index of a call made now in this sequence. Must be taken
     * synchronously at the call, while the caller's branch is the async context.
     */
    takeIndex() {
        const position = this.next === 0 && this.position.length > 0 ? this.position : [...this.position, this.next];
        this.next++;
        this.calls++;
        let index = positionIndex(position);
        // Two nested positions can hash alike. The later one takes the next free
        // index, so call ids stay unique; it can then only miss its replay.
        while (this.taken.has(index))
            index = index === Number.MAX_SAFE_INTEGER ? NESTED_INDEX_BASE : index + 1;
        this.taken.add(index);
        return index;
    }
    /** Sequences for the branches of a fan-out started here, one position each. */
    fork(count) {
        const first = this.next;
        this.next += count;
        return Array.from({ length: count }, (_, branch) => new CallSequence([...this.position, first + branch], this.taken));
    }
    /** Move past the calls of a settled fan-out's branches. */
    join(branches) {
        let made = 0;
        for (const branch of branches)
            made += branch.calls;
        this.next += made;
        this.calls += made;
    }
}

package pibridge

import (
	"encoding/json"
	"strings"

	"github.com/Autumn-27/norma/llm"
)

// Anthropic thinking blocks must be replayed with their signature, which the
// Chat Completions shape has no field for. Pi already round-trips the
// OpenRouter-style `reasoning_details` array: it keeps the whole array as the
// thinking block's replay signature and sends it back verbatim on the assistant
// message. The relay uses that channel:
//
//   - the visible thinking text streams as reasoning_content, as for any model;
//   - once, before the finish chunk, a delta (or the final message, when not
//     streaming) carries reasoning_details: one reasoning.text entry per signed
//     thinking block, with the complete signature;
//   - a request's assistant reasoning_details are mapped back to signed
//     thinking blocks.
//
// Pi treats reasoning_details as replay metadata and never shows it, so the
// thinking text is not duplicated.

const (
	detailText    = "reasoning.text"
	detailSummary = "reasoning.summary"
)

// reasoningDetail is one reasoning_details entry. The relay emits reasoning.text
// entries (text + signature) and empty reasoning.summary block separators.
type reasoningDetail struct {
	Type      string  `json:"type"`
	Text      string  `json:"text,omitempty"`
	Signature string  `json:"signature,omitempty"`
	Summary   *string `json:"summary,omitempty"`
}

// thinkingPiece is one thinking block being reassembled.
type thinkingPiece struct {
	text strings.Builder
	sig  string
}

type aggState int

const (
	aggIdle     aggState = iota // no thinking block in progress
	aggThinking                 // the last piece is still receiving thinking text
	aggSigned                   // the last piece has a signature (more fragments may follow)
)

// reasoningAgg reassembles thinking blocks (text + signature) from normalized
// stream events. Feed it thinking deltas, signature fragments and endBlock when
// any other content starts; details then returns the signed blocks.
//
// A signature may arrive in several fragments and is the concatenation of them.
// A thinking delta after a signature starts a new block.
type reasoningAgg struct {
	pieces []*thinkingPiece
	state  aggState
}

func (a *reasoningAgg) thinking(s string) {
	if s == "" {
		return
	}
	if a.state != aggThinking {
		a.pieces = append(a.pieces, &thinkingPiece{})
		a.state = aggThinking
	}
	a.pieces[len(a.pieces)-1].text.WriteString(s)
}

// signature appends a signature fragment to the current block. A signature with
// no thinking block in progress (an empty, display-omitted block) has nothing to
// attach to and is dropped, as the llm accumulator drops such a block.
func (a *reasoningAgg) signature(s string) {
	if s == "" || a.state == aggIdle {
		return
	}
	a.pieces[len(a.pieces)-1].sig += s
	a.state = aggSigned
}

// endBlock closes the current block: later thinking starts a new one and later
// signature fragments have nothing to attach to.
func (a *reasoningAgg) endBlock() { a.state = aggIdle }

// details renders the signed blocks as reasoning_details, or nil when there are
// none (non-Anthropic upstreams never sign, and then Pi's plain
// reasoning_content replay is all that is needed).
//
// Pi merges adjacent reasoning.text entries into one, keeping only the first
// signature, which would invalidate every signed block after the first. So
// blocks are separated by an empty reasoning.summary entry (Pi never merges
// across it; the request mapping ignores it).
func (a *reasoningAgg) details() []reasoningDetail {
	var out []reasoningDetail
	for _, p := range a.pieces {
		if p.sig == "" || p.text.Len() == 0 {
			continue
		}
		if len(out) > 0 {
			empty := ""
			out = append(out, reasoningDetail{Type: detailSummary, Summary: &empty})
		}
		out = append(out, reasoningDetail{Type: detailText, Text: p.text.String(), Signature: p.sig})
	}
	return out
}

// messageReasoningDetails is details() for a finished assistant message.
func messageReasoningDetails(msg llm.Message) []reasoningDetail {
	var a reasoningAgg
	for _, b := range msg.Content {
		if b.Type == llm.BlockThinking {
			a.thinking(b.Thinking)
			a.signature(b.Signature)
		}
		a.endBlock()
	}
	return a.details()
}

// thinkingBlocks restores the thinking of an assistant message from a request.
// Signed reasoning.text entries in reasoning_details win: they carry the
// signature Anthropic requires. Otherwise the plain reasoning field becomes one
// unsigned block.
func thinkingBlocks(msg chatMessage) []llm.ContentBlock {
	var details []reasoningDetail
	_ = json.Unmarshal(msg.ReasoningDetails, &details) // best effort: Pi only sends what the relay emitted
	var blocks []llm.ContentBlock
	for _, d := range details {
		if d.Type == detailText && d.Text != "" && d.Signature != "" {
			blocks = append(blocks, llm.ContentBlock{Type: llm.BlockThinking, Thinking: d.Text, Signature: d.Signature})
		}
	}
	if len(blocks) > 0 {
		return blocks
	}
	if r := firstNonEmpty(msg.ReasoningContent, msg.Reasoning, msg.ReasoningText); r != "" {
		return []llm.ContentBlock{{Type: llm.BlockThinking, Thinking: r}}
	}
	return nil
}

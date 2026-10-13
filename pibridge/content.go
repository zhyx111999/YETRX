package pibridge

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/Autumn-27/norma/llm"
)

// contentPart is one validated piece of message content: text, or an image.
type contentPart struct {
	text  string
	image *llm.ImageSource
}

// imageMediaTypes are the formats both Anthropic and OpenAI accept.
var imageMediaTypes = map[string]bool{"image/jpeg": true, "image/png": true, "image/gif": true, "image/webp": true}

// parseContent flattens an OpenAI content value (null | string | [parts]) into
// text and image parts. Images (image_url: a base64 data URL or an http(s) URL)
// are allowed only where allowImages is set — user messages, and tool messages
// whose images become part of the tool result; audio / file parts are rejected
// rather than silently dropped. The relay never fetches an image URL: it is
// handed to the provider as is.
func parseContent(raw json.RawMessage, where string, allowImages bool) ([]contentPart, *apiError) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	switch raw[0] {
	case '"':
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, badRequest("%s is invalid: %v", where, err)
		}
		return []contentPart{{text: s}}, nil
	case '[':
		var parts []struct {
			Type     string          `json:"type"`
			Text     *string         `json:"text"`
			ImageURL json.RawMessage `json:"image_url"`
		}
		if err := json.Unmarshal(raw, &parts); err != nil {
			return nil, badRequest("%s is invalid: %v", where, err)
		}
		out := make([]contentPart, 0, len(parts))
		for i, p := range parts {
			switch p.Type {
			case "text":
				if p.Text == nil {
					return nil, badRequest("%s[%d]: text part has no text", where, i)
				}
				out = append(out, contentPart{text: *p.Text})
			case "image_url":
				if !allowImages {
					return nil, badRequest("%s[%d]: image_url is only supported in user and tool messages", where, i)
				}
				src, err := parseImageURL(p.ImageURL, fmt.Sprintf("%s[%d]", where, i))
				if err != nil {
					return nil, err
				}
				out = append(out, contentPart{image: src})
			default:
				return nil, badRequest("%s[%d]: unsupported content part type %q", where, i, truncateRunes(p.Type, 64))
			}
		}
		return out, nil
	default:
		return nil, badRequest("%s must be a string or an array of content parts", where)
	}
}

// parseImageURL reads an image_url value — {"url": …} per the OpenAI spec, or a
// bare string — into an Anthropic-style image source. Error text never echoes
// the URL or its data: it may be megabytes of base64 or carry a credential.
func parseImageURL(raw json.RawMessage, where string) (*llm.ImageSource, *apiError) {
	raw = bytes.TrimSpace(raw)
	var u string
	switch {
	case len(raw) == 0 || string(raw) == "null":
		return nil, badRequest("%s: image_url part has no image_url", where)
	case raw[0] == '"':
		if err := json.Unmarshal(raw, &u); err != nil {
			return nil, badRequest("%s: image_url is invalid", where)
		}
	default:
		var obj struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, badRequest("%s: image_url is invalid", where)
		}
		u = obj.URL
	}
	u = strings.TrimSpace(u)
	if len(u) >= len("data:") && strings.EqualFold(u[:len("data:")], "data:") {
		return dataURLSource(u[len("data:"):], where)
	}
	if pu, err := url.Parse(u); err == nil && (pu.Scheme == "http" || pu.Scheme == "https") && pu.Host != "" {
		return &llm.ImageSource{Type: llm.ImageSourceURL, URL: u}, nil
	}
	return nil, badRequest("%s: image_url must be a base64 data URL or an http(s) URL", where)
}

// dataURLSource parses "<media type>;base64,<data>" (the part after "data:").
func dataURLSource(rest, where string) (*llm.ImageSource, *apiError) {
	meta, data, ok := strings.Cut(rest, ",")
	mediaType, b64 := strings.CutSuffix(strings.ToLower(strings.TrimSpace(meta)), ";base64")
	switch {
	case !ok || !b64:
		return nil, badRequest("%s: image data URL must be base64 (data:<type>;base64,<data>)", where)
	case !imageMediaTypes[mediaType]:
		return nil, badRequest("%s: unsupported image media type %q (use jpeg, png, gif or webp)", where, truncateRunes(mediaType, 64))
	case data == "" || !validBase64(data):
		return nil, badRequest("%s: image data is not valid base64", where)
	}
	return &llm.ImageSource{Type: llm.ImageSourceBase64, MediaType: mediaType, Data: data}, nil
}

// validBase64 reports whether s is standard padded base64 without line breaks
// (which Go's decoder would otherwise skip), decoding without allocating.
func validBase64(s string) bool {
	if strings.ContainsAny(s, "\r\n") {
		return false
	}
	_, err := io.Copy(io.Discard, base64.NewDecoder(base64.StdEncoding, strings.NewReader(s)))
	return err == nil
}

// texts returns the text of the parts that are text (system and assistant
// content never contains images: parseContent rejects them there).
func texts(parts []contentPart) []string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p.image == nil {
			out = append(out, p.text)
		}
	}
	return out
}

// userBlocks turns parts into content blocks in order: each non-empty text part
// is a text block (Anthropic rejects empty ones), each image an image block.
func userBlocks(parts []contentPart) []llm.ContentBlock {
	var blocks []llm.ContentBlock
	for _, p := range parts {
		switch {
		case p.image != nil:
			blocks = append(blocks, llm.ContentBlock{Type: llm.BlockImage, Source: p.image})
		case p.text != "":
			blocks = append(blocks, llm.TextBlock(p.text))
		}
	}
	return blocks
}

// toolResultBlocks builds a tool result's content: consecutive text parts join
// with "\n" into one text block, and images keep their place between them.
func toolResultBlocks(parts []contentPart) []llm.ContentBlock {
	var blocks []llm.ContentBlock
	var run []string
	flush := func() {
		if text := strings.Join(run, "\n"); text != "" {
			blocks = append(blocks, llm.TextBlock(text))
		}
		run = nil
	}
	for _, p := range parts {
		if p.image == nil {
			run = append(run, p.text)
			continue
		}
		flush()
		blocks = append(blocks, llm.ContentBlock{Type: llm.BlockImage, Source: p.image})
	}
	flush()
	return blocks
}

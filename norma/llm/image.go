package llm

// Image handling shared by the OpenAI-style encoders (Chat Completions and
// Responses). The Anthropic adapter needs none of this: ContentBlock already
// marshals to Anthropic's image / tool_result shape.
//
// Neither OpenAI wire format lets a tool result carry an image, so the images
// of a tool_result are lifted out of it. The tool message / function_call_output
// keeps the text, and one user message with the images follows the group of
// tool results (the same shape Pi itself uses on Chat Completions).

const (
	// toolResultImageNote introduces the user message that carries the images
	// lifted out of tool results.
	toolResultImageNote = "Attached image(s) from tool result:"
	// toolResultImageOnly is the tool output when a tool result is only images:
	// an empty tool message / function_call_output is rejected by the APIs.
	toolResultImageOnly = "(see attached image)"
)

// imageURL renders the source as the URL string OpenAI-style endpoints take: the
// http(s) URL itself, or a data: URL for base64 data.
func (s *ImageSource) imageURL() string {
	if s.Type == ImageSourceURL {
		return s.URL
	}
	return "data:" + s.MediaType + ";base64," + s.Data
}

// toolResultParts splits a tool_result into the text its OpenAI tool message
// carries and the images to lift into the follow-up user message.
func toolResultParts(b ContentBlock) (text string, images []*ImageSource) {
	for _, c := range b.Content {
		if c.Type == BlockImage && c.Source != nil {
			images = append(images, c.Source)
		}
	}
	text = flattenText(b.Content)
	if text == "" && len(images) > 0 {
		text = toolResultImageOnly
	}
	return text, images
}

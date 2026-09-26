package agent

import "strings"

var inlineThinkingOpenTags = []string{
	"<think>",
	"<thinking>",
	"<reasoning>",
	"<antml:thinking>",
}

var inlineThinkingCloseTags = []string{
	"</think>",
	"</thinking>",
	"</reasoning>",
	"</antml:thinking>",
}

// thinkingFilter removes inline reasoning tags from a streamed text response.
// It holds partial tags between chunks so a tag split across SSE events cannot
// leak into the transcript.
type thinkingFilter struct {
	pending string
	hidden  bool
}

func (f *thinkingFilter) Feed(text string) string {
	f.pending += text
	var visible strings.Builder

	for f.pending != "" {
		if f.hidden {
			index, tag := findTag(f.pending, inlineThinkingCloseTags)
			if index < 0 {
				keep := partialTagSuffix(f.pending, inlineThinkingCloseTags)
				f.pending = f.pending[len(f.pending)-keep:]
				return visible.String()
			}
			f.pending = f.pending[index+len(tag):]
			f.hidden = false
			continue
		}

		index, tag := findTag(f.pending, inlineThinkingOpenTags)
		if index < 0 {
			keep := partialTagSuffix(f.pending, inlineThinkingOpenTags)
			visible.WriteString(f.pending[:len(f.pending)-keep])
			f.pending = f.pending[len(f.pending)-keep:]
			return visible.String()
		}
		visible.WriteString(f.pending[:index])
		f.pending = f.pending[index+len(tag):]
		f.hidden = true
	}

	return visible.String()
}

func (f *thinkingFilter) Flush() string {
	if f.hidden {
		f.pending = ""
		return ""
	}
	visible := f.pending
	f.pending = ""
	return visible
}

func findTag(text string, tags []string) (int, string) {
	index := -1
	found := ""
	for _, tag := range tags {
		if i := strings.Index(text, tag); i >= 0 && (index < 0 || i < index) {
			index = i
			found = tag
		}
	}
	return index, found
}

func partialTagSuffix(text string, tags []string) int {
	keep := 0
	for _, tag := range tags {
		max := len(tag) - 1
		if max > len(text) {
			max = len(text)
		}
		for n := max; n > keep; n-- {
			if strings.HasSuffix(text, tag[:n]) {
				keep = n
				break
			}
		}
	}
	return keep
}

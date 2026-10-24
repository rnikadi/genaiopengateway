package guardrail

import (
	"bytes"
	"encoding/json"
	"maps"
	"slices"
	"strings"
)

// Doc is a JSON body whose text can be checked and replaced: the
// messages, prompts and tool arguments of model requests and responses
// in the OpenAI, Anthropic and Gemini formats.
type Doc struct {
	root any
	refs []ref
}

// ref locates one string in the decoded body.
type ref struct {
	m   map[string]any
	key string
	a   []any
	i   int
}

func (r ref) get() string {
	if r.m != nil {
		s, _ := r.m[r.key].(string)
		return s
	}
	s, _ := r.a[r.i].(string)
	return s
}

func (r ref) set(s string) {
	if r.m != nil {
		r.m[r.key] = s
	} else {
		r.a[r.i] = s
	}
}

// textFields hold text, directly or within arrays and objects.
var textFields = map[string]bool{
	"content": true, "text": true, "input": true, "system": true, "instructions": true,
	"output": true, "prompt": true, "arguments": true,
	// Rerank requests.
	"query": true, "documents": true,
}

// skipFields hold definitions and settings, not conversation text.
var skipFields = map[string]bool{
	"tools": true, "functions": true, "tool_choice": true, "response_format": true,
	"metadata": true, "usage": true, "tool_config": true, "toolConfig": true,
}

// argumentFields hold tool arguments when their value is an object, and
// then every string inside is text. ("input" also holds the Responses
// API's list of items, which is walked like any other list.)
var argumentFields = map[string]bool{"input": true, "arguments": true, "args": true}

// ParseDoc decodes a JSON body. It fails when the body is not JSON.
func ParseDoc(raw []byte) (*Doc, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	d := &Doc{root: v}
	d.walk(v, false)
	return d, nil
}

// ArgumentsDoc wraps decoded tool arguments, every string of which is text.
func ArgumentsDoc(args any) *Doc {
	d := &Doc{root: args}
	d.walk(args, true)
	return d
}

// walk collects the text in v. Under all, every string is text; else
// only strings in text fields, and in arrays held by text fields.
func (d *Doc) walk(v any, all bool) {
	switch t := v.(type) {
	case map[string]any:
		// Sorted, so the same body always gives the same order.
		for _, k := range slices.Sorted(maps.Keys(t)) {
			if !all && skipFields[k] {
				continue
			}
			switch x := t[k].(type) {
			case string:
				if all || textFields[k] {
					d.add(ref{m: t, key: k}, x)
				}
			case map[string]any:
				d.walk(x, all || argumentFields[k])
			case []any:
				for i, e := range x {
					if s, ok := e.(string); ok {
						if all || textFields[k] {
							d.add(ref{a: x, i: i}, s)
						}
						continue
					}
					d.walk(e, all)
				}
			}
		}
	case []any:
		for i, x := range t {
			if s, ok := x.(string); ok {
				if all {
					d.add(ref{a: t, i: i}, s)
				}
				continue
			}
			d.walk(x, all)
		}
	}
}

func (d *Doc) add(r ref, s string) {
	// Inline media is not text.
	if s == "" || strings.HasPrefix(s, "data:") {
		return
	}
	d.refs = append(d.refs, r)
}

// Texts returns the texts found, in a stable order for one Doc.
func (d *Doc) Texts() []string {
	out := make([]string, len(d.refs))
	for i, r := range d.refs {
		out[i] = r.get()
	}
	return out
}

// SetTexts replaces the texts, which must be as many as Texts returned.
func (d *Doc) SetTexts(texts []string) {
	for i, r := range d.refs {
		if i < len(texts) {
			r.set(texts[i])
		}
	}
}

// Value returns the decoded body, with any replacements.
func (d *Doc) Value() any { return d.root }

// JSON encodes the body, with any replacements.
func (d *Doc) JSON() []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(d.root)
	return bytes.TrimRight(buf.Bytes(), "\n")
}

package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// hasDuplicateKeys reports whether a JSON document repeats a key in any
// object. Parsers disagree on which of the repeated values wins, so a
// message the gateway checks must not have any: what the gateway saw
// could differ from what the upstream acts on. Invalid JSON reports false.
func hasDuplicateKeys(data []byte) bool {
	dec := json.NewDecoder(bytes.NewReader(data))
	type frame struct {
		keys   map[string]bool
		object bool
		key    bool // the next string token is a key
	}
	var stack []frame
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) || err != nil {
			return false
		}
		top := len(stack) - 1
		// A value inside an object is followed by a key again.
		afterValue := func() {
			if top >= 0 && stack[top].object {
				stack[top].key = true
			}
		}
		switch v := tok.(type) {
		case json.Delim:
			switch v {
			case '{':
				if top >= 0 && stack[top].object {
					stack[top].key = false
				}
				stack = append(stack, frame{keys: map[string]bool{}, object: true, key: true})
			case '[':
				if top >= 0 && stack[top].object {
					stack[top].key = false
				}
				stack = append(stack, frame{})
			case '}', ']':
				stack = stack[:top]
				if top-1 >= 0 && stack[top-1].object {
					stack[top-1].key = true
				}
			}
		case string:
			if top >= 0 && stack[top].object && stack[top].key {
				if stack[top].keys[v] {
					return true
				}
				stack[top].keys[v] = true
				stack[top].key = false
				continue
			}
			afterValue()
		default:
			afterValue()
		}
	}
}

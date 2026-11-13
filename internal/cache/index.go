package cache

import (
	"container/list"
	"encoding/json"
	"math"
	"strings"
	"sync"
	"time"
)

// Index holds question embeddings for semantic matches, in this process,
// oldest first out. Each vector points at the key of a stored answer and
// belongs to a context: everything in the request but the question.
type Index struct {
	mu    sync.Mutex
	max   int
	order *list.List // of *vector, newest first
	byCtx map[string][]*list.Element
	now   func() time.Time
}

type vector struct {
	ctx, key string
	v        []float32
	expires  time.Time
}

// NewIndex returns an index of up to max vectors.
func NewIndex(max int) *Index {
	return &Index{max: max, order: list.New(), byCtx: map[string][]*list.Element{}, now: time.Now}
}

// SetMax changes how many vectors the index holds, dropping the oldest
// when it shrinks.
func (x *Index) SetMax(max int) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.max = max
	for x.order.Len() > x.max {
		x.remove(x.order.Back())
	}
}

// Add records that the question v, in context ctx, is answered by the
// entry at key until ttl passes.
func (x *Index) Add(ctx, key string, v []float32, ttl time.Duration) {
	n := normalize(v)
	if n == nil {
		return
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	el := x.order.PushFront(&vector{ctx: ctx, key: key, v: n, expires: x.now().Add(ttl)})
	x.byCtx[ctx] = append(x.byCtx[ctx], el)
	for x.order.Len() > x.max {
		x.remove(x.order.Back())
	}
}

// Nearest returns the key of the closest question in ctx with cosine
// similarity of at least threshold.
func (x *Index) Nearest(ctx string, v []float32, threshold float64) (string, float64, bool) {
	n := normalize(v)
	if n == nil {
		return "", 0, false
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	now := x.now()
	best, bestKey := -1.0, ""
	for _, el := range append([]*list.Element(nil), x.byCtx[ctx]...) {
		vec := el.Value.(*vector)
		if !now.Before(vec.expires) {
			x.remove(el)
			continue
		}
		if len(vec.v) != len(n) {
			continue
		}
		var dot float64
		for i := range n {
			dot += float64(n[i]) * float64(vec.v[i])
		}
		if dot > best {
			best, bestKey = dot, vec.key
		}
	}
	return bestKey, best, bestKey != "" && best >= threshold
}

// Clear removes the vectors of contexts that start with prefix.
func (x *Index) Clear(prefix string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	for ctx, els := range x.byCtx {
		if strings.HasPrefix(ctx, prefix) {
			for _, el := range els {
				x.order.Remove(el)
			}
			delete(x.byCtx, ctx)
		}
	}
}

func (x *Index) remove(el *list.Element) {
	vec := el.Value.(*vector)
	x.order.Remove(el)
	els := x.byCtx[vec.ctx]
	for i, e := range els {
		if e == el {
			els = append(els[:i], els[i+1:]...)
			break
		}
	}
	if len(els) == 0 {
		delete(x.byCtx, vec.ctx)
	} else {
		x.byCtx[vec.ctx] = els
	}
}

func normalize(v []float32) []float32 {
	var sum float64
	for _, f := range v {
		sum += float64(f) * float64(f)
	}
	if sum == 0 {
		return nil
	}
	norm := math.Sqrt(sum)
	out := make([]float32, len(v))
	for i, f := range v {
		out[i] = float32(float64(f) / norm)
	}
	return out
}

// Question splits a chat completions or Messages body into the text of
// its last message, when that is the user's, and the body without it.
// It reports false when the last message is not a user's text.
func Question(body []byte) (string, []byte, bool) {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return "", nil, false
	}
	var msgs []json.RawMessage
	if json.Unmarshal(m["messages"], &msgs) != nil || len(msgs) == 0 {
		return "", nil, false
	}
	var last struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(msgs[len(msgs)-1], &last) != nil || last.Role != "user" {
		return "", nil, false
	}
	text := contentText(last.Content)
	if strings.TrimSpace(text) == "" {
		return "", nil, false
	}
	m["messages"], _ = json.Marshal(msgs[:len(msgs)-1])
	rest, err := json.Marshal(m)
	if err != nil {
		return "", nil, false
	}
	return text, rest, true
}

// contentText is the text of a message's content: a string, or the text
// parts of a list. A list with other parts, such as images, has no text
// to match on.
func contentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var texts []string
	for _, p := range parts {
		if p.Type != "text" {
			return ""
		}
		texts = append(texts, p.Text)
	}
	return strings.Join(texts, "\n")
}

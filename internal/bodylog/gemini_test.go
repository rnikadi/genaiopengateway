package bodylog

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestAssembleGemini(t *testing.T) {
	stream := `data: {"candidates":[{"content":{"role":"model","parts":[{"text":"think","thought":true},{"text":"Hel"}]}}],"responseId":"r"}` + "\n\n" +
		`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"lo"},{"functionCall":{"name":"f"}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":2}}` + "\n\n"
	got := string(Assemble("text/event-stream", []byte(stream)))
	want := `{"candidates":[{"content":{"parts":[{"text":"think","thought":true},{"text":"Hello"},{"functionCall":{"name":"f"}}],"role":"model"},"finishReason":"STOP"}],"responseId":"r","usageMetadata":{"promptTokenCount":2}}`
	if got != want {
		t.Errorf("assembled\n got %s\nwant %s", got, want)
	}

	// A whole response is a stream of one event.
	ev, ok := Stream([]byte(want))
	if !ok {
		t.Fatal("not recognized")
	}
	var a, b any
	_ = json.Unmarshal([]byte(want), &a)
	_ = json.Unmarshal(Assemble("text/event-stream", ev), &b)
	if !reflect.DeepEqual(a, b) {
		t.Errorf("round trip: %s", ev)
	}
}

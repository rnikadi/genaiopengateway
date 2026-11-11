package native

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestMessage(t *testing.T) {
	for body, want := range map[string]string{
		`{"error":{"message":"bad key"}}`:                              "bad key",
		`{"detail":{"status":"invalid_api_key","message":"nope"}}`:     "nope",
		`{"detail":[{"msg":"field required"},{"msg":"too long"}]}`:     "field required; too long",
		`{"err_code":"INVALID_AUTH","err_msg":"Invalid credentials."}`: "Invalid credentials.",
		`{"error":"Authentication error"}`:                             "Authentication error",
		`upstream exploded`:                                            "upstream exploded",
	} {
		if got := Message([]byte(body)); got != want {
			t.Errorf("Message(%s) = %q, want %q", body, got, want)
		}
	}
}

func TestTranslateError(t *testing.T) {
	resp := &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": {"7"}}, Body: io.NopCloser(strings.NewReader(`{"message":"slow down"}`))}
	out := TranslateError(resp)
	b, _ := io.ReadAll(out.Body)
	if out.StatusCode != 429 || out.Header.Get("Retry-After") != "7" || !strings.Contains(string(b), `"type":"rate_limit_error"`) || !strings.Contains(string(b), "slow down") {
		t.Fatalf("%d %v %s", out.StatusCode, out.Header, b)
	}
}

func TestTranscriptFormats(t *testing.T) {
	tr := Transcript{Text: "hello world", Language: "en", Duration: 61.5, Words: []Word{{"hello", 0, 0.5}, {"world", 0.6, 61.5}}}
	read := func(format string) string {
		r := tr.Respond(format)
		if r.Usage.AudioSeconds != 61.5 {
			t.Errorf("%s: audio seconds %v", format, r.Usage.AudioSeconds)
		}
		b, _ := io.ReadAll(r.Body)
		return string(b)
	}
	if got := read(""); !strings.Contains(got, `"text":"hello world"`) || !strings.Contains(got, `"seconds":61.5`) {
		t.Errorf("json: %s", got)
	}
	if got := read("text"); got != "hello world\n" {
		t.Errorf("text: %q", got)
	}
	if got := read("verbose_json"); !strings.Contains(got, `"duration":61.5`) || !strings.Contains(got, `"language":"en"`) || !strings.Contains(got, `{"word":"world","start":0.6,"end":61.5}`) {
		t.Errorf("verbose_json: %s", got)
	}
	if got := read("srt"); got != "1\n00:00:00,000 --> 00:01:01,500\nhello world\n\n" {
		t.Errorf("srt: %q", got)
	}
	if got := read("vtt"); !strings.HasPrefix(got, "WEBVTT\n\n00:00:00.000 --> 00:01:01.500\n") {
		t.Errorf("vtt: %q", got)
	}
	if CheckTranscriptFormat("diarized_json") == nil || CheckTranscriptFormat("srt") != nil {
		t.Error("format check")
	}
}

func TestFields(t *testing.T) {
	f := Fields([]byte(`{"model":"m","language":"de","timestamp_granularities[]":["word","segment"]}`))
	if f["model"] != "m" || f["language"] != "de" || f["timestamp_granularities[]"] != "word" {
		t.Fatal(f)
	}
}

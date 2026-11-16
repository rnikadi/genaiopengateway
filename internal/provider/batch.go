package provider

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/usage"
)

// This file is what adapters share to serve OpenAI's Batch API on a
// cloud's own batch jobs (docs/batch.md): reading the uploaded input
// file, naming its rows, and writing the file, batch and result objects
// clients read.

// BatchChatURL is the only request URL native batch jobs serve.
const BatchChatURL = "/v1/chat/completions"

// BatchLine is one request of a batch input file.
type BatchLine struct {
	CustomID string          `json:"custom_id"`
	URL      string          `json:"url"`
	Body     json.RawMessage `json:"body"`
}

// BatchUpload is the input file of a POST /files call.
type BatchUpload struct {
	Filename string
	// Data is the file as uploaded.
	Data  []byte
	Lines []BatchLine
	// Model is the upstream model every line names.
	Model string
}

// ReadBatchUpload reads the JSONL file of a multipart upload of at most
// limit bytes. Every line must be a chat completion.
func ReadBatchUpload(body io.Reader, contentType string, limit int64) (*BatchUpload, error) {
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil || params["boundary"] == "" || body == nil {
		return nil, errors.New("upload the file as multipart/form-data")
	}
	mr := multipart.NewReader(io.LimitReader(body, limit), params["boundary"])
	up := &BatchUpload{}
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, errors.New("could not read the multipart body")
		}
		if part.FormName() != "file" {
			continue
		}
		up.Filename = part.FileName()
		if up.Data, err = io.ReadAll(part); err != nil {
			return nil, errors.New("could not read the file")
		}
	}
	for i, raw := range bytes.Split(up.Data, []byte("\n")) {
		raw = bytes.TrimSpace(raw)
		if len(raw) == 0 {
			continue
		}
		var l BatchLine
		var m struct {
			Model string `json:"model"`
		}
		if json.Unmarshal(raw, &l) != nil || json.Unmarshal(l.Body, &m) != nil || m.Model == "" {
			return nil, fmt.Errorf("line %d: not a request with a body and a model", i+1)
		}
		if l.URL != BatchChatURL {
			return nil, fmt.Errorf("line %d (%s): this provider's batches serve only %s", i+1, l.CustomID, BatchChatURL)
		}
		up.Model = m.Model
		up.Lines = append(up.Lines, l)
	}
	if len(up.Lines) == 0 {
		return nil, errors.New("the file has no requests")
	}
	if up.Filename == "" {
		up.Filename = "batch.jsonl"
	}
	return up, nil
}

// BatchRow names row i of an input file in a form every cloud accepts
// as a record id or a label value.
func BatchRow(i int) string { return fmt.Sprintf("r%010d", i) }

// BatchSource is a client's input file, kept beside the job so results
// can be given back under the client's custom ids.
type BatchSource struct {
	ids, models []string
}

// ParseBatchSource reads an input file ReadBatchUpload accepted.
func ParseBatchSource(data []byte) *BatchSource {
	s := &BatchSource{}
	for _, raw := range bytes.Split(data, []byte("\n")) {
		if raw = bytes.TrimSpace(raw); len(raw) == 0 {
			continue
		}
		var l struct {
			CustomID string `json:"custom_id"`
			Body     struct {
				Model string `json:"model"`
			} `json:"body"`
		}
		_ = json.Unmarshal(raw, &l)
		s.ids = append(s.ids, l.CustomID)
		s.models = append(s.models, l.Body.Model)
	}
	return s
}

// Row returns the custom id and model of the row BatchRow named, or
// empty strings for a row the file does not have.
func (s *BatchSource) Row(row string) (customID, model string) {
	i, err := strconv.Atoi(strings.TrimPrefix(row, "r"))
	if err != nil || !strings.HasPrefix(row, "r") || i < 0 || i >= len(s.ids) {
		return "", ""
	}
	return s.ids[i], s.models[i]
}

// BatchResult is a line of a batch output file: one request's answer.
func BatchResult(row, customID string, body any) []byte {
	b, _ := json.Marshal(map[string]any{
		"id": "batch_req_" + row, "custom_id": customID, "error": nil,
		"response": map[string]any{"status_code": http.StatusOK, "request_id": row, "body": body},
	})
	return append(b, '\n')
}

// BatchFailure is a line of a batch error file: one request's failure.
func BatchFailure(row, customID, code, message string) []byte {
	b, _ := json.Marshal(map[string]any{
		"id": "batch_req_" + row, "custom_id": customID, "response": nil,
		"error": map[string]string{"code": code, "message": message},
	})
	return append(b, '\n')
}

// BatchFileID returns a new id for an input file: 24 hex digits, which
// are safe in object names and job names.
func BatchFileID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// BatchFileMeta describes an input file. Adapters store it beside the
// file, since a batch is created from the file's id alone.
type BatchFileMeta struct {
	Filename string `json:"filename"`
	Bytes    int    `json:"bytes"`
	Created  int64  `json:"created_at"`
	Model    string `json:"model"`
	Requests int64  `json:"requests"`
}

// FileObject is the OpenAI file object of an input or output file.
func FileObject(id, purpose string, m BatchFileMeta) []byte {
	b, _ := json.Marshal(map[string]any{
		"id": id, "object": "file", "purpose": purpose, "filename": m.Filename, "bytes": m.Bytes, "created_at": m.Created,
	})
	return b
}

// BatchState is a cloud batch job as an OpenAI batch.
type BatchState struct {
	ID, InputFileID, Status   string
	Created                   int64
	OutputFileID, ErrorFileID string
	Total, Completed, Failed  int64
	// Tokens is what the job used, once the cloud reports it.
	Tokens *usage.Tokens
	// Message says why a job failed.
	Message string
}

// Object is the OpenAI batch object.
func (s BatchState) Object() []byte {
	if s.Created == 0 {
		s.Created = time.Now().Unix()
	}
	o := map[string]any{
		"id": s.ID, "object": "batch", "endpoint": BatchChatURL, "input_file_id": s.InputFileID,
		"completion_window": "24h", "status": s.Status, "created_at": s.Created,
		"output_file_id": nil, "error_file_id": nil, "errors": nil,
		"request_counts": map[string]int64{"total": s.Total, "completed": s.Completed, "failed": s.Failed},
	}
	if s.OutputFileID != "" {
		o["output_file_id"] = s.OutputFileID
	}
	if s.ErrorFileID != "" {
		o["error_file_id"] = s.ErrorFileID
	}
	if s.Message != "" {
		o["errors"] = map[string]any{"object": "list", "data": []map[string]string{{"code": "batch_failed", "message": s.Message}}}
	}
	if t := s.Tokens; t != nil {
		o["usage"] = map[string]any{
			"input_tokens": t.InputTokens, "output_tokens": t.OutputTokens, "total_tokens": t.Total(),
			"input_tokens_details":  map[string]int64{"cached_tokens": t.CachedInputTokens},
			"output_tokens_details": map[string]int64{"reasoning_tokens": t.ReasoningTokens},
		}
	}
	b, _ := json.Marshal(o)
	return b
}

// JSONReply is a JSON answer to a forwarded call.
func JSONReply(status int, body []byte) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(body))}
}

// ErrorReply is an OpenAI-shaped error answer to a forwarded call.
func ErrorReply(status int, typ, msg string) *http.Response {
	b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": msg, "type": typ, "code": typ}})
	return JSONReply(status, b)
}

package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"

	"github.com/rnikadi/genaiopengateway/internal/provider"
)

func isMultipart(r *http.Request) bool {
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return mt == "multipart/form-data"
}

// readMultipart reads a multipart body into its text fields, as a JSON
// object of strings (lists for repeated fields), and its files. "stream"
// becomes a boolean, as in JSON requests.
func readMultipart(r *http.Request, raw []byte) (map[string]json.RawMessage, []provider.File, error) {
	_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || params["boundary"] == "" {
		return nil, nil, errors.New("no boundary")
	}
	mr := multipart.NewReader(bytes.NewReader(raw), params["boundary"])
	values := map[string][]string{}
	var order []string
	files := []provider.File{}
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, err
		}
		data, err := io.ReadAll(part)
		if err != nil {
			return nil, nil, err
		}
		name := part.FormName()
		if name == "" {
			continue
		}
		if part.FileName() != "" {
			files = append(files, provider.File{Field: name, Filename: part.FileName(), ContentType: part.Header.Get("Content-Type"), Data: data})
			continue
		}
		if _, seen := values[name]; !seen {
			order = append(order, name)
		}
		values[name] = append(values[name], string(data))
	}
	body := map[string]json.RawMessage{}
	for _, name := range order {
		v := values[name]
		switch {
		case name == "stream" && (v[0] == "true" || v[0] == "false"):
			body[name] = json.RawMessage(v[0])
		case len(v) == 1:
			body[name], _ = json.Marshal(v[0])
		default:
			body[name], _ = json.Marshal(v)
		}
	}
	return body, files, nil
}

// withFileNotes is a multipart request as body logging keeps it: the
// text fields, and each file's name, type and size instead of its bytes.
func withFileNotes(body map[string]json.RawMessage, files []provider.File) []byte {
	notes := make([]map[string]any, len(files))
	for i, f := range files {
		notes[i] = map[string]any{"field": f.Field, "filename": f.Filename, "content_type": f.ContentType, "bytes": len(f.Data)}
	}
	out := map[string]any{"files": notes}
	for k, v := range body {
		out[k] = v
	}
	raw, _ := json.Marshal(out)
	return raw
}

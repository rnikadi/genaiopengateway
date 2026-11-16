package vertex

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/provider"
	"github.com/rnikadi/genaiopengateway/internal/usage"
)

// Veo on Vertex AI (docs/modalities.md). A job is a long-running
// operation of the model: predictLongRunning starts it and
// fetchPredictOperation reads it. The finished video comes back in the
// operation, or, when the provider has a Cloud Storage folder, is
// written there and read from there.

// videoPrefix starts the ids of Veo jobs on Vertex AI: the operation's
// name, encoded so it fits one path segment.
const videoPrefix = "vveo_"

func isVeo(model string) bool { return strings.HasPrefix(model, "veo-") }

// modelPath is the resource name of a Google model in the provider's
// project and location.
func (p *Provider) modelPath(model string) string {
	return "projects/" + p.cfg.Project + "/locations/" + p.cfg.Location + "/publishers/google/models/" + model
}

// operationOf returns the operation and the model a video id names, or
// "" when the id is not one of this provider's Veo operations: an id
// cannot make the gateway call another project or another resource.
func (p *Provider) operationOf(id string) (op, model string) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(id, videoPrefix))
	op = string(raw)
	rest, ok := strings.CutPrefix(op, p.modelPath(""))
	model, opID, cut := strings.Cut(rest, "/operations/")
	if !strings.HasPrefix(id, videoPrefix) || err != nil || !ok || !cut || !isVeo(model) || opID == "" ||
		strings.ContainsAny(model+opID, "/?#%") || strings.Contains(op, "..") {
		return "", ""
	}
	return op, model
}

// veoSize maps an OpenAI size to Veo's aspect ratio and resolution.
func veoSize(size string) (ratio, resolution string) {
	w, h, ok := strings.Cut(size, "x")
	wi, err1 := strconv.Atoi(w)
	hi, err2 := strconv.Atoi(h)
	if !ok || err1 != nil || err2 != nil || wi <= 0 || hi <= 0 {
		return "", ""
	}
	ratio, short := "16:9", hi
	if hi > wi {
		ratio, short = "9:16", wi
	}
	resolution = "720p"
	if short >= 1080 {
		resolution = "1080p"
	}
	return ratio, resolution
}

// videoObject is the OpenAI video job shape.
func videoObject(id, model, status string, progress int, seconds, size string, created int64, code, message string) map[string]any {
	obj := map[string]any{"id": id, "object": "video", "model": model, "status": status, "progress": progress, "created_at": created, "seconds": seconds}
	if size != "" {
		obj["size"] = size
	}
	if message != "" {
		obj["error"] = map[string]string{"code": code, "message": message}
	}
	return obj
}

// video starts a Veo job and answers the OpenAI job object. Image to
// video and remix are not offered.
func (p *Provider) video(ctx context.Context, req *provider.Request, model string) (*provider.Response, error) {
	if req.Files != nil {
		return errorResponse(http.StatusBadRequest, "invalid_request_error", "Veo targets take a prompt only, without input_reference"), nil
	}
	var vr struct {
		Prompt  string          `json:"prompt"`
		Seconds json.RawMessage `json:"seconds"`
		Size    string          `json:"size"`
	}
	if json.Unmarshal(req.Body, &vr) != nil || vr.Prompt == "" {
		return errorResponse(http.StatusBadRequest, "invalid_request_error", "prompt is required"), nil
	}
	seconds := 8
	if len(vr.Seconds) > 0 {
		n, err := strconv.Atoi(strings.Trim(string(vr.Seconds), `"`))
		if err != nil || n < 1 || n > 60 {
			return errorResponse(http.StatusBadRequest, "invalid_request_error", "seconds must be a whole number of seconds"), nil
		}
		seconds = n
	}
	params := map[string]any{"durationSeconds": seconds, "sampleCount": 1}
	if ratio, res := veoSize(vr.Size); ratio != "" {
		params["aspectRatio"], params["resolution"] = ratio, res
	}
	if p.cfg.GCSOutputURI != "" {
		params["storageUri"] = strings.TrimRight(p.cfg.GCSOutputURI, "/") + "/"
	}
	body, _ := json.Marshal(map[string]any{"instances": []any{map[string]string{"prompt": vr.Prompt}}, "parameters": params})
	status, hdr, buf, err := p.call(ctx, http.MethodPost, p.base+"/v1/"+p.modelPath(url.PathEscape(model))+":predictLongRunning", body, nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return googleError(status, hdr, buf), nil
	}
	var op struct {
		Name string `json:"name"`
	}
	id := ""
	if json.Unmarshal(buf, &op) == nil && op.Name != "" {
		id = videoPrefix + base64.RawURLEncoding.EncodeToString([]byte(op.Name))
	}
	if name, _ := p.operationOf(id); name == "" {
		return errorResponse(http.StatusBadGateway, "api_error", "the upstream did not return an operation of this model"), nil
	}
	b, _ := json.Marshal(videoObject(id, req.Model, "queued", 0, strconv.Itoa(seconds), vr.Size, time.Now().Unix(), "", ""))
	return &provider.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(b)),
		Usage:      &usage.Tokens{VideoSeconds: float64(seconds), Reported: true},
	}, nil
}

// operation is a Veo long-running operation on Vertex AI.
type operation struct {
	Done  bool `json:"done"`
	Error *struct {
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
	Response struct {
		Videos []struct {
			GCSURI string `json:"gcsUri"`
			Bytes  string `json:"bytesBase64Encoded"`
			Mime   string `json:"mimeType"`
		} `json:"videos"`
		Filtered []string `json:"raiMediaFilteredReasons"`
	} `json:"response"`
}

// Forward implements provider.Forwarder for batches (batch.go) and for
// Veo jobs: retrieve reads the operation, content returns the video,
// and delete forgets the job, which Vertex AI has no call to delete.
func (p *Provider) Forward(ctx context.Context, method, path string, in io.Reader, contentType string) (*http.Response, error) {
	if path == "/files" || path == "/batches" || strings.HasPrefix(path, "/files/") || strings.HasPrefix(path, "/batches/") {
		return p.forwardBatch(ctx, method, path, in, contentType)
	}
	rest, ok := strings.CutPrefix(path, "/videos/")
	if !ok {
		return jsonReply(http.StatusNotFound, errorJSON("not_found", "Vertex AI serves only batches and Veo jobs here")), nil
	}
	rest, _, _ = strings.Cut(rest, "?")
	id, suffix, _ := strings.Cut(rest, "/")
	id, _ = url.PathUnescape(id)
	opName, model := p.operationOf(id)
	if opName == "" || (suffix != "" && suffix != "content") {
		return jsonReply(http.StatusNotFound, errorJSON("not_found", "no such video")), nil
	}
	if method == http.MethodDelete {
		return jsonReply(http.StatusOK, map[string]any{"id": id, "object": "video.deleted", "deleted": true}), nil
	}
	body, _ := json.Marshal(map[string]string{"operationName": opName})
	status, hdr, buf, err := p.call(ctx, http.MethodPost, p.base+"/v1/"+p.modelPath(url.PathEscape(model))+":fetchPredictOperation", body, nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		out := googleError(status, hdr, buf)
		return &http.Response{StatusCode: out.StatusCode, Header: out.Header, Body: out.Body}, nil
	}
	var op operation
	if json.Unmarshal(buf, &op) != nil {
		return jsonReply(http.StatusBadGateway, errorJSON("api_error", "the upstream answer is not an operation")), nil
	}
	if suffix == "" {
		state, progress, code, message := "in_progress", 50, "", ""
		switch {
		case op.Error != nil:
			state, code, message = "failed", strings.ToLower(op.Error.Status), op.Error.Message
		case op.Done && len(op.Response.Videos) == 0:
			state, code, message = "failed", "filtered", "the video was filtered: "+strings.Join(op.Response.Filtered, "; ")
		case op.Done:
			state, progress = "completed", 100
		}
		return jsonReply(http.StatusOK, videoObject(id, model, state, progress, "", "", 0, code, message)), nil
	}
	if !op.Done || len(op.Response.Videos) == 0 {
		return jsonReply(http.StatusNotFound, errorJSON("video_not_ready", "the video is not ready")), nil
	}
	v := op.Response.Videos[0]
	mime := v.Mime
	if mime == "" {
		mime = "video/mp4"
	}
	if v.Bytes != "" {
		raw, err := base64.StdEncoding.DecodeString(v.Bytes)
		if err != nil {
			return jsonReply(http.StatusBadGateway, errorJSON("api_error", "the upstream's video could not be read")), nil
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {mime}, "Content-Length": {strconv.Itoa(len(raw))}}, Body: io.NopCloser(bytes.NewReader(raw))}, nil
	}
	return p.download(ctx, v.GCSURI, mime)
}

// download streams a video from the provider's Cloud Storage folder.
// An object anywhere else is refused, so a changed answer cannot make
// the gateway read other objects with its credentials.
func (p *Provider) download(ctx context.Context, uri, mime string) (*http.Response, error) {
	folder := strings.TrimRight(p.cfg.GCSOutputURI, "/") + "/"
	if p.cfg.GCSOutputURI == "" || !strings.HasPrefix(uri, folder) || strings.Contains(uri, "..") {
		return jsonReply(http.StatusBadGateway, errorJSON("api_error", "the video is not in the provider's gcs_output_uri")), nil
	}
	bucket, object, _ := strings.Cut(strings.TrimPrefix(uri, "gs://"), "/")
	ctx, cancel := context.WithTimeout(ctx, p.cfg.Timeout.Std())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.gcsBase+"/storage/v1/b/"+url.PathEscape(bucket)+"/o/"+url.PathEscape(object)+"?alt=media", nil)
	if err != nil {
		cancel()
		return nil, err
	}
	if err := p.auth.authorize(req); err != nil {
		cancel()
		return jsonReply(http.StatusBadGateway, errorJSON("api_error", "upstream credentials: "+err.Error())), nil
	}
	resp, err := p.client.Do(req)
	if err != nil {
		cancel()
		return nil, err
	}
	resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
	if resp.StatusCode == http.StatusOK {
		resp.Header.Set("Content-Type", mime)
	}
	return resp, nil
}

// cancelOnClose releases a request's context when its body is closed.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnClose) Close() error {
	defer c.cancel()
	return c.ReadCloser.Close()
}

func errorJSON(code, msg string) map[string]any {
	return map[string]any{"error": map[string]any{"message": msg, "type": code, "code": code}}
}

func jsonReply(status int, v any) *http.Response {
	b, _ := json.Marshal(v)
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(b))}
}

package gemini

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

// videoPrefix starts the ids of Veo jobs: the operation name, encoded
// so it fits one path segment.
const videoPrefix = "veo_"

func isVeo(model string) bool { return strings.HasPrefix(strings.TrimPrefix(model, "models/"), "veo-") }

func videoID(operation string) string {
	return videoPrefix + base64.RawURLEncoding.EncodeToString([]byte(operation))
}

// operationOf returns the operation a video id names, or "" when it is
// not a Veo id or names something other than a model operation.
func operationOf(id string) string {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(id, videoPrefix))
	op := string(raw)
	if !strings.HasPrefix(id, videoPrefix) || err != nil || !strings.HasPrefix(op, "models/") || !strings.Contains(op, "/operations/") || strings.Contains(op, "..") || strings.ContainsAny(op, "?#") {
		return ""
	}
	return op
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
func videoObject(id, model, status string, progress int, seconds, size string, created int64, failure *googleError) map[string]any {
	obj := map[string]any{"id": id, "object": "video", "model": model, "status": status, "progress": progress, "created_at": created, "seconds": seconds}
	if size != "" {
		obj["size"] = size
	}
	if failure != nil {
		obj["error"] = map[string]string{"code": strings.ToLower(failure.Error.Status), "message": failure.Error.Message}
	}
	return obj
}

// video starts a Veo job with predictLongRunning and answers the OpenAI
// job object. Image to video and remix are not offered.
func (p *Provider) video(ctx context.Context, req *provider.Request) (*provider.Response, error) {
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
	params := map[string]any{"durationSeconds": seconds}
	if ratio, res := veoSize(vr.Size); ratio != "" {
		params["aspectRatio"], params["resolution"] = ratio, res
	}
	body, _ := json.Marshal(map[string]any{"instances": []any{map[string]string{"prompt": vr.Prompt}}, "parameters": params})
	resp, err := p.call(ctx, req.Model, "predictLongRunning", body, false)
	if err != nil || resp.StatusCode != http.StatusOK {
		if err == nil {
			return translateError(resp)
		}
		return resp, err
	}
	buf, err := readAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var op struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(buf, &op) != nil || op.Name == "" {
		return errorResponse(http.StatusBadGateway, "api_error", "the upstream did not return an operation"), nil
	}
	obj := videoObject(videoID(op.Name), req.Model, "queued", 0, strconv.Itoa(seconds), vr.Size, time.Now().Unix(), nil)
	b, _ := json.Marshal(obj)
	resp.Header = http.Header{"Content-Type": {"application/json"}}
	resp.Body = io.NopCloser(bytes.NewReader(b))
	*resp.Usage = usage.Tokens{VideoSeconds: float64(seconds), Reported: true}
	return resp, nil
}

// operation is a Veo long-running operation.
type operation struct {
	Name  string `json:"name"`
	Done  bool   `json:"done"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
	Response struct {
		Video struct {
			Samples []struct {
				Video struct {
					URI string `json:"uri"`
				} `json:"video"`
			} `json:"generatedSamples"`
			Filtered []string `json:"raiMediaFilteredReasons"`
		} `json:"generateVideoResponse"`
	} `json:"response"`
}

// Forward implements provider.Forwarder for Veo jobs: retrieve polls
// the operation, content downloads the video, and delete forgets the
// job, which Veo does not store beyond two days.
func (p *Provider) Forward(ctx context.Context, method, path string, _ io.Reader, _ string) (*http.Response, error) {
	rest, ok := strings.CutPrefix(path, "/videos/")
	if !ok {
		return jsonReply(http.StatusNotFound, errorBody("not_found", "Gemini serves only Veo jobs here")), nil
	}
	rest, _, _ = strings.Cut(rest, "?")
	id, suffix, _ := strings.Cut(rest, "/")
	id, _ = url.PathUnescape(id)
	opName := operationOf(id)
	if opName == "" || (suffix != "" && suffix != "content") {
		return jsonReply(http.StatusNotFound, errorBody("not_found", "no such video")), nil
	}
	if method == http.MethodDelete {
		return jsonReply(http.StatusOK, map[string]any{"id": id, "object": "video.deleted", "deleted": true}), nil
	}
	op, resp, err := p.poll(ctx, opName)
	if err != nil || resp != nil {
		return resp, err
	}
	model := strings.Split(strings.TrimPrefix(opName, "models/"), "/")[0]
	if suffix == "" {
		status, progress := "in_progress", 50
		var failure *googleError
		switch {
		case op.Error != nil:
			status, failure = "failed", &googleError{}
			failure.Error.Message, failure.Error.Status = op.Error.Message, op.Error.Status
		case op.Done && len(op.Response.Video.Samples) == 0:
			status, failure = "failed", &googleError{}
			failure.Error.Message, failure.Error.Status = "the video was filtered: "+strings.Join(op.Response.Video.Filtered, "; "), "FILTERED"
		case op.Done:
			status, progress = "completed", 100
		}
		return jsonReply(http.StatusOK, videoObject(id, model, status, progress, "", "", 0, failure)), nil
	}
	if !op.Done || len(op.Response.Video.Samples) == 0 || op.Response.Video.Samples[0].Video.URI == "" {
		return jsonReply(http.StatusNotFound, errorBody("video_not_ready", "the video is not ready")), nil
	}
	return p.download(ctx, op.Response.Video.Samples[0].Video.URI)
}

// poll reads an operation. A non-nil response is an error to return.
func (p *Provider) poll(ctx context.Context, name string) (*operation, *http.Response, error) {
	resp, err := p.get(ctx, p.cfg.BaseURL+"/v1beta/"+name)
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, resp, nil
	}
	buf, err := readAll(resp.Body)
	if err != nil {
		return nil, nil, err
	}
	var op operation
	if json.Unmarshal(buf, &op) != nil {
		return nil, jsonReply(http.StatusBadGateway, errorBody("api_error", "the upstream answer is not an operation")), nil
	}
	return &op, nil, nil
}

// download streams a generated video from the Gemini API's file
// service, which needs the API key. Other hosts are refused, so a
// changed answer cannot send the key elsewhere.
func (p *Provider) download(ctx context.Context, uri string) (*http.Response, error) {
	u, err := url.Parse(uri)
	base, _ := url.Parse(p.cfg.BaseURL)
	if err != nil || base == nil || u.Scheme != base.Scheme || u.Host != base.Host {
		return jsonReply(http.StatusBadGateway, errorBody("api_error", "the video is on a host the gateway does not send keys to")), nil
	}
	resp, err := p.get(ctx, uri)
	if err != nil || resp.StatusCode != http.StatusOK {
		return resp, err
	}
	if resp.Header.Get("Content-Type") == "" || strings.HasPrefix(resp.Header.Get("Content-Type"), "application/octet-stream") {
		resp.Header.Set("Content-Type", "video/mp4")
	}
	return resp, nil
}

// get sends a GET with the API key. The body is read within the
// provider's timeout.
func (p *Provider) get(ctx context.Context, u string) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, p.cfg.Timeout.Std())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	for k, v := range p.cfg.Headers {
		req.Header.Set(k, v)
	}
	if key := p.cfg.APIKey.Value(); key != "" {
		req.Header.Set("X-Goog-Api-Key", key)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		cancel()
		return nil, err
	}
	resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

func errorBody(code, msg string) map[string]any {
	return map[string]any{"error": map[string]any{"message": msg, "type": code, "code": code}}
}

func jsonReply(status int, v any) *http.Response {
	b, _ := json.Marshal(v)
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(b))}
}

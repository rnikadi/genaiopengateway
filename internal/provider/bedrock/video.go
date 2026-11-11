package bedrock

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

// reelPrefix starts the ids of Nova Reel jobs: the invocation ARN,
// encoded so it fits one path segment.
const reelPrefix = "reel_"

func isReel(model string) bool { return strings.Contains(model, "amazon.nova-reel") }

func reelID(arn string) string { return reelPrefix + base64.RawURLEncoding.EncodeToString([]byte(arn)) }

// arnOf returns the async invocation ARN a video id names, or "".
func arnOf(id string) string {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(id, reelPrefix))
	arn := string(raw)
	if !strings.HasPrefix(id, reelPrefix) || err != nil || !strings.HasPrefix(arn, "arn:aws") || !strings.Contains(arn, ":async-invoke/") {
		return ""
	}
	return arn
}

// video starts a Nova Reel job with StartAsyncInvoke. Six seconds is
// one shot; longer videos, in multiples of six up to two minutes, are
// automated multi-shot videos.
func (p *Provider) video(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	if req.Files != nil {
		return errorResponse(http.StatusBadRequest, "invalid_request_error", "Nova Reel targets take a prompt only, without input_reference"), nil
	}
	var vr struct {
		Prompt  string          `json:"prompt"`
		Seconds json.RawMessage `json:"seconds"`
		Size    string          `json:"size"`
	}
	if json.Unmarshal(req.Body, &vr) != nil || vr.Prompt == "" {
		return errorResponse(http.StatusBadRequest, "invalid_request_error", "prompt is required"), nil
	}
	seconds := 6
	if len(vr.Seconds) > 0 {
		n, err := strconv.Atoi(strings.Trim(string(vr.Seconds), `"`))
		if err != nil || n < 6 || n > 120 || n%6 != 0 {
			return errorResponse(http.StatusBadRequest, "invalid_request_error", "seconds must be 6, or a multiple of 6 up to 120, on Nova Reel"), nil
		}
		seconds = n
	}
	if vr.Size != "" && vr.Size != "1280x720" {
		return errorResponse(http.StatusBadRequest, "invalid_request_error", "Nova Reel makes 1280x720 videos"), nil
	}
	input := map[string]any{
		"taskType":              "TEXT_VIDEO",
		"textToVideoParams":     map[string]string{"text": vr.Prompt},
		"videoGenerationConfig": map[string]any{"durationSeconds": seconds, "fps": 24, "dimension": "1280x720"},
	}
	if seconds > 6 {
		input["taskType"] = "MULTI_SHOT_AUTOMATED"
		input["multiShotAutomatedParams"] = map[string]string{"text": vr.Prompt}
		delete(input, "textToVideoParams")
	}
	body, _ := json.Marshal(map[string]any{
		"modelId":          req.Model,
		"modelInput":       input,
		"outputDataConfig": map[string]any{"s3OutputDataConfig": map[string]string{"s3Uri": p.cfg.S3OutputURI}},
	})
	resp, err := p.call(ctx, http.MethodPost, p.base+"/async-invoke", body)
	if err != nil || resp.StatusCode != http.StatusOK {
		if err == nil {
			return openAIError(resp), nil
		}
		return resp, err
	}
	buf, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}
	var out struct {
		ARN string `json:"invocationArn"`
	}
	if json.Unmarshal(buf, &out) != nil || out.ARN == "" {
		return errorResponse(http.StatusBadGateway, "api_error", "the upstream did not return an invocation"), nil
	}
	obj := map[string]any{"id": reelID(out.ARN), "object": "video", "model": req.Model, "status": "queued", "progress": 0,
		"created_at": time.Now().Unix(), "seconds": strconv.Itoa(seconds), "size": "1280x720"}
	b, _ := json.Marshal(obj)
	return &provider.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(bytes.NewReader(b)), Usage: &usage.Tokens{VideoSeconds: float64(seconds), Reported: true}}, nil
}

// call sends a request to Bedrock with the provider's credentials.
func (p *Provider) call(ctx context.Context, method, u string, body []byte) (*provider.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, p.cfg.Timeout.Std())
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	hreq, err := http.NewRequestWithContext(ctx, method, u, r)
	if err != nil {
		cancel()
		return nil, err
	}
	if body != nil {
		hreq.Header.Set("Content-Type", "application/json")
	}
	for k, v := range p.cfg.Headers {
		hreq.Header.Set(k, v)
	}
	if err := p.auth.authorize(hreq); err != nil {
		cancel()
		return errorResponse(http.StatusBadGateway, "api_error", "upstream credentials: "+err.Error()), nil
	}
	resp, err := p.client.Do(hreq)
	if err != nil {
		cancel()
		return nil, err
	}
	return &provider.Response{StatusCode: resp.StatusCode, Header: resp.Header, Body: &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}, Usage: &usage.Tokens{}}, nil
}

// invocation is a GetAsyncInvoke answer.
type invocation struct {
	Status  string `json:"status"`
	Failure string `json:"failureMessage"`
	Output  struct {
		S3 struct {
			URI string `json:"s3Uri"`
		} `json:"s3OutputDataConfig"`
	} `json:"outputDataConfig"`
}

// Forward implements provider.Forwarder for Nova Reel jobs: retrieve
// reads the invocation, content reads output.mp4 from S3, and delete
// forgets the job, leaving the video in the bucket.
func (p *Provider) Forward(ctx context.Context, method, path string, _ io.Reader, _ string) (*http.Response, error) {
	rest, ok := strings.CutPrefix(path, "/videos/")
	if !ok {
		return jsonReply(http.StatusNotFound, "Bedrock serves only Nova Reel jobs here"), nil
	}
	rest, _, _ = strings.Cut(rest, "?")
	id, suffix, _ := strings.Cut(rest, "/")
	id, _ = url.PathUnescape(id)
	arn := arnOf(id)
	if arn == "" || (suffix != "" && suffix != "content") {
		return jsonReply(http.StatusNotFound, "no such video"), nil
	}
	if method == http.MethodDelete {
		b, _ := json.Marshal(map[string]any{"id": id, "object": "video.deleted", "deleted": true})
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(b))}, nil
	}
	resp, err := p.call(ctx, http.MethodGet, p.base+"/async-invoke/"+url.PathEscape(arn), nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		e := openAIError(resp)
		return &http.Response{StatusCode: e.StatusCode, Header: e.Header, Body: e.Body}, nil
	}
	buf, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}
	var inv invocation
	if json.Unmarshal(buf, &inv) != nil {
		return jsonReply(http.StatusBadGateway, "the upstream answer is not an invocation"), nil
	}
	if suffix == "" {
		obj := map[string]any{"id": id, "object": "video", "status": "in_progress", "progress": 50}
		switch inv.Status {
		case "Completed":
			obj["status"], obj["progress"] = "completed", 100
		case "Failed":
			obj["status"] = "failed"
			obj["error"] = map[string]string{"code": "generation_failed", "message": inv.Failure}
		}
		b, _ := json.Marshal(obj)
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(b))}, nil
	}
	if inv.Status != "Completed" {
		return jsonReply(http.StatusNotFound, "the video is not ready"), nil
	}
	return p.download(ctx, inv.Output.S3.URI, arn[strings.LastIndex(arn, "/")+1:])
}

// download reads output.mp4 below the invocation's S3 folder.
func (p *Provider) download(ctx context.Context, s3URI, invocationID string) (*http.Response, error) {
	if s3URI == "" {
		s3URI = p.cfg.S3OutputURI
	}
	bucket, prefix, ok := strings.Cut(strings.TrimPrefix(s3URI, "s3://"), "/")
	if !strings.HasPrefix(s3URI, "s3://") || bucket == "" {
		return jsonReply(http.StatusBadGateway, "the invocation has no S3 output"), nil
	}
	prefix = strings.Trim(prefix, "/")
	if !ok || !strings.HasSuffix("/"+prefix, "/"+invocationID) {
		// Bedrock writes each job to a folder named by its invocation id.
		prefix = strings.TrimPrefix(prefix+"/"+invocationID, "/")
	}
	key := prefix + "/output.mp4"
	ctx, cancel := context.WithTimeout(ctx, p.cfg.Timeout.Std())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.s3URL(bucket, (&url.URL{Path: key}).EscapedPath()), nil)
	if err != nil {
		cancel()
		return nil, err
	}
	if err := p.auth.signS3(req); err != nil {
		cancel()
		return jsonReply(http.StatusNotImplemented, "reading the video from S3 needs AWS credentials from the default chain: "+err.Error()), nil
	}
	resp, err := p.client.Do(req)
	if err != nil {
		cancel()
		return nil, err
	}
	resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
	if resp.StatusCode == http.StatusOK {
		resp.Header.Set("Content-Type", "video/mp4")
	}
	return resp, nil
}

func jsonReply(status int, msg string) *http.Response {
	b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": msg, "type": "not_found", "code": "not_found"}})
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(b))}
}

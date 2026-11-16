package vertex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/provider"
	"github.com/rnikadi/genaiopengateway/internal/provider/gemini"
	"github.com/rnikadi/genaiopengateway/internal/usage"
)

// OpenAI's Batch API on Vertex AI batch prediction for Gemini models
// (docs/batch.md). An uploaded file becomes three objects below
// <gcs_output_uri>/batches/<file id>/: source.jsonl as the client sent
// it, input.jsonl with each request translated to generateContent, and
// meta.json. A batch is a batch prediction job on input.jsonl, which
// writes below out/. The ids the gateway hands out name the file and
// the job, so nothing else is stored.
//
// Vertex AI returns results in any order and has no request id, so each
// request carries its row in a label, which the result echoes.

// rowLabel is the label that carries a request's row.
const rowLabel = "gog_row"

var (
	batchFileID = regexp.MustCompile(`^file_vx_([0-9a-f]{24})$`)
	batchOutID  = regexp.MustCompile(`^file_vx(out|err)_([0-9]{1,24})_([0-9a-f]{24})$`)
	batchJobID  = regexp.MustCompile(`^batch_vx_([0-9]{1,24})_([0-9a-f]{24})$`)
)

// maxBatchFile bounds a batch file read into memory.
const maxBatchFile = 512 << 20

// batchTotals caches what finished jobs did, by job.
var batchTotals sync.Map

// totals is what a finished job did.
type totals struct {
	completed, failed int64
	tokens            usage.Tokens
}

// batchFolder returns the bucket and object prefix of a file's folder.
func (p *Provider) batchFolder(fid string) (bucket, prefix string) {
	bucket, prefix, _ = strings.Cut(strings.TrimPrefix(p.cfg.GCSOutputURI, "gs://"), "/")
	return bucket, strings.TrimPrefix(strings.Trim(prefix, "/")+"/batches/"+fid+"/", "/")
}

// gcs calls Cloud Storage. The caller closes the response body.
func (p *Provider) gcs(ctx context.Context, method, u string, body []byte) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, p.cfg.Timeout.Std())
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		cancel()
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	if err := p.auth.authorize(req); err != nil {
		cancel()
		return provider.ErrorReply(http.StatusBadGateway, "api_error", "upstream credentials: "+err.Error()), nil
	}
	resp, err := p.client.Do(req)
	if err != nil {
		cancel()
		return nil, err
	}
	resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

func (p *Provider) objectURL(bucket, object string) string {
	return p.gcsBase + "/storage/v1/b/" + url.PathEscape(bucket) + "/o/" + url.PathEscape(object)
}

// gcsFailure is a Cloud Storage error as a gateway answer.
func gcsFailure(resp *http.Response) *http.Response {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	msg := strings.TrimSpace(string(b))
	if json.Unmarshal(b, &e) == nil && e.Error.Message != "" {
		msg = e.Error.Message
	}
	return provider.ErrorReply(http.StatusBadGateway, "api_error", "Cloud Storage answered "+resp.Status+": "+msg)
}

// gcsRead reads a whole object. A missing object is found false.
func (p *Provider) gcsRead(ctx context.Context, bucket, object string) (data []byte, found bool, failure *http.Response, err error) {
	resp, err := p.gcs(ctx, http.MethodGet, p.objectURL(bucket, object)+"?alt=media", nil)
	if err != nil {
		return nil, false, nil, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, false, nil, nil
	case resp.StatusCode != http.StatusOK:
		return nil, false, gcsFailure(resp), nil
	}
	data, err = io.ReadAll(io.LimitReader(resp.Body, maxBatchFile))
	return data, err == nil, nil, err
}

// forwardBatch serves the files and batches calls of provider.Forwarder.
func (p *Provider) forwardBatch(ctx context.Context, method, path string, body io.Reader, contentType string) (*http.Response, error) {
	if p.cfg.GCSOutputURI == "" {
		return provider.ErrorReply(http.StatusBadRequest, "invalid_request_error", "this Vertex AI provider has no gcs_output_uri, which batches need"), nil
	}
	path, _, _ = strings.Cut(path, "?")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	id := ""
	if len(parts) > 1 {
		id, _ = url.PathUnescape(parts[1])
	}
	switch {
	case path == "/files" && method == http.MethodPost:
		return p.batchUpload(ctx, body, contentType)
	case path == "/batches" && method == http.MethodPost:
		return p.batchCreate(ctx, body)
	case parts[0] == "files" && len(parts) == 2 && method == http.MethodGet:
		return p.batchFile(ctx, id)
	case parts[0] == "files" && len(parts) == 2 && method == http.MethodDelete:
		return p.batchDelete(ctx, id)
	case parts[0] == "files" && len(parts) == 3 && parts[2] == "content" && method == http.MethodGet:
		return p.batchContent(ctx, id)
	case parts[0] == "batches" && len(parts) == 2 && method == http.MethodGet:
		return p.batchGet(ctx, id, false)
	case parts[0] == "batches" && len(parts) == 3 && parts[2] == "cancel" && method == http.MethodPost:
		return p.batchGet(ctx, id, true)
	}
	return provider.ErrorReply(http.StatusNotFound, "not_found", "Vertex AI batches do not serve "+method+" "+path), nil
}

// batchUpload stores an input file and its generateContent translation.
func (p *Provider) batchUpload(ctx context.Context, body io.Reader, contentType string) (*http.Response, error) {
	up, err := provider.ReadBatchUpload(body, contentType, maxBatchFile)
	if err != nil {
		return provider.ErrorReply(http.StatusBadRequest, "invalid_request_error", err.Error()), nil
	}
	if pub, _ := Split(up.Model); pub != google_ {
		return provider.ErrorReply(http.StatusBadRequest, "invalid_request_error", "Vertex AI batches are served for Gemini models only"), nil
	}
	var input bytes.Buffer
	for i, l := range up.Lines {
		gr, err := gemini.ChatToGenerate(l.Body, map[string]string{rowLabel: provider.BatchRow(i)})
		if err != nil {
			return provider.ErrorReply(http.StatusBadRequest, "invalid_request_error", "request "+l.CustomID+": "+err.Error()), nil
		}
		rec, err := json.Marshal(map[string]json.RawMessage{"request": gr})
		if err != nil {
			return nil, err
		}
		input.Write(rec)
		input.WriteByte('\n')
	}
	fid := provider.BatchFileID()
	meta := provider.BatchFileMeta{Filename: up.Filename, Bytes: len(up.Data), Created: time.Now().Unix(), Model: up.Model, Requests: int64(len(up.Lines))}
	mb, _ := json.Marshal(meta)
	bucket, prefix := p.batchFolder(fid)
	// meta.json goes last: a file without it does not exist.
	for _, o := range []struct {
		name string
		data []byte
	}{{"source.jsonl", up.Data}, {"input.jsonl", input.Bytes()}, {"meta.json", mb}} {
		u := p.gcsBase + "/upload/storage/v1/b/" + url.PathEscape(bucket) + "/o?uploadType=media&name=" + url.QueryEscape(prefix+o.name)
		resp, err := p.gcs(ctx, http.MethodPost, u, o.data)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			defer resp.Body.Close()
			return gcsFailure(resp), nil
		}
		_ = resp.Body.Close()
	}
	return provider.JSONReply(http.StatusOK, provider.FileObject("file_vx_"+fid, "batch", meta)), nil
}

// batchMeta reads an input file's description.
func (p *Provider) batchMeta(ctx context.Context, fid string) (*provider.BatchFileMeta, *http.Response, error) {
	bucket, prefix := p.batchFolder(fid)
	data, found, failure, err := p.gcsRead(ctx, bucket, prefix+"meta.json")
	if err != nil || failure != nil {
		return nil, failure, err
	}
	var m provider.BatchFileMeta
	if !found || json.Unmarshal(data, &m) != nil {
		return nil, provider.ErrorReply(http.StatusNotFound, "not_found", "no such file"), nil
	}
	return &m, nil, nil
}

// batchFile retrieves a file object.
func (p *Provider) batchFile(ctx context.Context, id string) (*http.Response, error) {
	if m := batchOutID.FindStringSubmatch(id); m != nil {
		return provider.JSONReply(http.StatusOK, provider.FileObject(id, "batch_output", provider.BatchFileMeta{Filename: m[1] + ".jsonl"})), nil
	}
	m := batchFileID.FindStringSubmatch(id)
	if m == nil {
		return provider.ErrorReply(http.StatusNotFound, "not_found", "no such file"), nil
	}
	meta, failure, err := p.batchMeta(ctx, m[1])
	if err != nil || failure != nil {
		return failure, err
	}
	return provider.JSONReply(http.StatusOK, provider.FileObject(id, "batch", *meta)), nil
}

// batchDelete removes an input file from Cloud Storage. Result files
// stay in the bucket: the output and error files of a job are the same
// objects there.
func (p *Provider) batchDelete(ctx context.Context, id string) (*http.Response, error) {
	deleted, _ := json.Marshal(map[string]any{"id": id, "object": "file", "deleted": true})
	if batchOutID.MatchString(id) {
		return provider.JSONReply(http.StatusOK, deleted), nil
	}
	m := batchFileID.FindStringSubmatch(id)
	if m == nil {
		return provider.ErrorReply(http.StatusNotFound, "not_found", "no such file"), nil
	}
	bucket, prefix := p.batchFolder(m[1])
	for _, name := range []string{"meta.json", "input.jsonl", "source.jsonl"} {
		resp, err := p.gcs(ctx, http.MethodDelete, p.objectURL(bucket, prefix+name), nil)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotFound {
			defer resp.Body.Close()
			return gcsFailure(resp), nil
		}
		_ = resp.Body.Close()
	}
	return provider.JSONReply(http.StatusOK, deleted), nil
}

// jobsURL is the URL of the location's batch prediction jobs.
func (p *Provider) jobsURL() string {
	return p.base + "/v1/projects/" + url.PathEscape(p.cfg.Project) + "/locations/" + url.PathEscape(p.cfg.Location) + "/batchPredictionJobs"
}

// reply turns a Google API answer that is not a success into a
// forwarded answer.
func reply(status int, hdr http.Header, buf []byte) *http.Response {
	e := googleError(status, hdr, buf)
	return &http.Response{StatusCode: e.StatusCode, Header: e.Header, Body: e.Body}
}

// batchCreate starts a batch prediction job on an input file.
func (p *Provider) batchCreate(ctx context.Context, body io.Reader) (*http.Response, error) {
	var req struct {
		InputFileID string `json:"input_file_id"`
	}
	if body == nil || json.NewDecoder(io.LimitReader(body, 1<<20)).Decode(&req) != nil {
		return provider.ErrorReply(http.StatusBadRequest, "invalid_request_error", "request body must be a JSON object"), nil
	}
	m := batchFileID.FindStringSubmatch(req.InputFileID)
	if m == nil {
		return provider.ErrorReply(http.StatusNotFound, "not_found", "no such file"), nil
	}
	fid := m[1]
	meta, failure, err := p.batchMeta(ctx, fid)
	if err != nil || failure != nil {
		return failure, err
	}
	bucket, prefix := p.batchFolder(fid)
	folder := "gs://" + bucket + "/" + prefix
	_, name := Split(meta.Model)
	job, _ := json.Marshal(map[string]any{
		"displayName":  "gog-" + fid,
		"model":        "publishers/google/models/" + name,
		"inputConfig":  map[string]any{"instancesFormat": "jsonl", "gcsSource": map[string]any{"uris": []string{folder + "input.jsonl"}}},
		"outputConfig": map[string]any{"predictionsFormat": "jsonl", "gcsDestination": map[string]string{"outputUriPrefix": folder + "out/"}},
	})
	status, hdr, buf, err := p.call(ctx, http.MethodPost, p.jobsURL(), job, nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return reply(status, hdr, buf), nil
	}
	var created struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(buf, &created)
	id := "batch_vx_" + created.Name[strings.LastIndex(created.Name, "/")+1:] + "_" + fid
	if !batchJobID.MatchString(id) {
		return provider.ErrorReply(http.StatusBadGateway, "api_error", "the upstream answer names no job"), nil
	}
	return provider.JSONReply(http.StatusOK, provider.BatchState{ID: id, InputFileID: req.InputFileID, Status: "validating", Total: meta.Requests}.Object()), nil
}

// batchStatus maps a batch prediction job state to OpenAI's status.
var batchStatus = map[string]string{
	"JOB_STATE_QUEUED": "validating", "JOB_STATE_PENDING": "validating",
	"JOB_STATE_RUNNING": "in_progress", "JOB_STATE_UPDATING": "in_progress", "JOB_STATE_PAUSED": "in_progress",
	"JOB_STATE_SUCCEEDED": "completed", "JOB_STATE_PARTIALLY_SUCCEEDED": "completed", "JOB_STATE_FAILED": "failed",
	"JOB_STATE_CANCELLING": "cancelling", "JOB_STATE_CANCELLED": "cancelled", "JOB_STATE_EXPIRED": "expired",
}

// batchJob is a batch prediction job.
type batchJob struct {
	State      string `json:"state"`
	CreateTime string `json:"createTime"`
	Error      *struct {
		Message string `json:"message"`
	} `json:"error"`
	OutputInfo struct {
		Directory string `json:"gcsOutputDirectory"`
	} `json:"outputInfo"`
	Stats struct {
		Successful string `json:"successfulCount"`
		Failed     string `json:"failedCount"`
		Incomplete string `json:"incompleteCount"`
	} `json:"completionStats"`
}

// job reads a batch prediction job.
func (p *Provider) job(ctx context.Context, jobID string) (*batchJob, *http.Response, error) {
	status, hdr, buf, err := p.call(ctx, http.MethodGet, p.jobsURL()+"/"+jobID, nil, nil)
	if err != nil {
		return nil, nil, err
	}
	if status != http.StatusOK {
		return nil, reply(status, hdr, buf), nil
	}
	var j batchJob
	if json.Unmarshal(buf, &j) != nil || batchStatus[j.State] == "" {
		return nil, provider.ErrorReply(http.StatusBadGateway, "api_error", "the upstream answer is not a batch prediction job"), nil
	}
	return &j, nil, nil
}

// batchGet retrieves a batch, cancelling its job first when cancel is
// set.
func (p *Provider) batchGet(ctx context.Context, id string, cancel bool) (*http.Response, error) {
	m := batchJobID.FindStringSubmatch(id)
	if m == nil {
		return provider.ErrorReply(http.StatusNotFound, "not_found", "no such batch"), nil
	}
	jobID, fid := m[1], m[2]
	if cancel {
		status, hdr, buf, err := p.call(ctx, http.MethodPost, p.jobsURL()+"/"+jobID+":cancel", []byte("{}"), nil)
		if err != nil {
			return nil, err
		}
		if status != http.StatusOK {
			return reply(status, hdr, buf), nil
		}
	}
	j, failure, err := p.job(ctx, jobID)
	if err != nil || failure != nil {
		return failure, err
	}
	st := provider.BatchState{ID: id, InputFileID: "file_vx_" + fid, Status: batchStatus[j.State]}
	if t, err := time.Parse(time.RFC3339, j.CreateTime); err == nil {
		st.Created = t.Unix()
	}
	st.Completed, _ = strconv.ParseInt(j.Stats.Successful, 10, 64)
	st.Failed, _ = strconv.ParseInt(j.Stats.Failed, 10, 64)
	incomplete, _ := strconv.ParseInt(j.Stats.Incomplete, 10, 64)
	st.Total = st.Completed + st.Failed + incomplete
	if st.Status == "failed" && j.Error != nil {
		st.Message = j.Error.Message
	}
	// A cancelled or expired job may have answered some requests, which
	// Google bills, so its results are counted too.
	if dir := p.outputDir(j, fid); dir != "" && (st.Status == "completed" || st.Status == "cancelled" || st.Status == "expired") {
		key := p.cfg.GCSOutputURI + "|" + jobID
		v, ok := batchTotals.Load(key)
		if !ok {
			var t totals
			failure, err := p.results(ctx, dir, func(r result) error {
				t.tokens.InputTokens += r.tokens.InputTokens
				t.tokens.OutputTokens += r.tokens.OutputTokens
				t.tokens.CachedInputTokens += r.tokens.CachedInputTokens
				t.tokens.ReasoningTokens += r.tokens.ReasoningTokens
				if r.failure != "" {
					t.failed++
				} else {
					t.completed++
				}
				return nil
			})
			if err != nil || failure != nil {
				return failure, err
			}
			t.tokens.Reported = true
			v = &t
			batchTotals.Store(key, v)
		}
		t := v.(*totals)
		st.Completed, st.Failed, st.Tokens = t.completed, t.failed, &t.tokens
		st.Total = max(st.Total, t.completed+t.failed)
		if t.completed > 0 {
			st.OutputFileID = "file_vxout_" + jobID + "_" + fid
		}
		if t.failed > 0 {
			st.ErrorFileID = "file_vxerr_" + jobID + "_" + fid
		}
	}
	return provider.JSONReply(http.StatusOK, st.Object()), nil
}

// outputDir returns the object prefix a job wrote its results below, or
// "" when it names none inside the file's folder: an answer cannot make
// the gateway read other objects with its credentials.
func (p *Provider) outputDir(j *batchJob, fid string) string {
	bucket, prefix := p.batchFolder(fid)
	dir := strings.TrimRight(j.OutputInfo.Directory, "/") + "/"
	if !strings.HasPrefix(dir, "gs://"+bucket+"/"+prefix+"out/") || strings.Contains(dir, "..") {
		return ""
	}
	return strings.TrimPrefix(dir, "gs://"+bucket+"/")
}

// result is one request's result in a job's output.
type result struct {
	row      string
	response json.RawMessage
	// failure says why the request has no response.
	failure string
	tokens  usage.Tokens
}

// results calls fn with each result below a job's output folder.
func (p *Provider) results(ctx context.Context, dir string, fn func(result) error) (*http.Response, error) {
	bucket, _ := p.batchFolder("")
	token := ""
	for {
		u := p.gcsBase + "/storage/v1/b/" + url.PathEscape(bucket) + "/o?prefix=" + url.QueryEscape(dir) + "&fields=items(name),nextPageToken"
		if token != "" {
			u += "&pageToken=" + url.QueryEscape(token)
		}
		status, hdr, buf, err := p.call(ctx, http.MethodGet, u, nil, nil)
		if err != nil {
			return nil, err
		}
		if status != http.StatusOK {
			return reply(status, hdr, buf), nil
		}
		var list struct {
			Items []struct {
				Name string `json:"name"`
			} `json:"items"`
			Next string `json:"nextPageToken"`
		}
		if json.Unmarshal(buf, &list) != nil {
			return provider.ErrorReply(http.StatusBadGateway, "api_error", "Cloud Storage returned an unreadable listing"), nil
		}
		for _, it := range list.Items {
			if !strings.HasSuffix(it.Name, ".jsonl") || !strings.HasPrefix(it.Name, dir) {
				continue
			}
			resp, err := p.gcs(ctx, http.MethodGet, p.objectURL(bucket, it.Name)+"?alt=media", nil)
			if err != nil {
				return nil, err
			}
			if resp.StatusCode != http.StatusOK {
				defer resp.Body.Close()
				return gcsFailure(resp), nil
			}
			err = readResults(resp.Body, fn)
			_ = resp.Body.Close()
			if err != nil {
				return nil, err
			}
		}
		if token = list.Next; token == "" {
			return nil, nil
		}
	}
}

// readResults reads the lines of one results object.
func readResults(r io.Reader, fn func(result) error) error {
	rd := bufio.NewReaderSize(r, 64<<10)
	for {
		line, err := rd.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			var l struct {
				Status  string `json:"status"`
				Request struct {
					Labels map[string]string `json:"labels"`
				} `json:"request"`
				Response json.RawMessage `json:"response"`
			}
			if json.Unmarshal(line, &l) == nil {
				res := result{row: l.Request.Labels[rowLabel], response: l.Response}
				switch {
				case l.Status != "":
					res.failure = l.Status
				case len(l.Response) == 0 || string(l.Response) == "null":
					res.failure = "the job returned no response"
				default:
					_, res.tokens, res.failure, _ = gemini.GenerateToChat(l.Response, "")
				}
				if ferr := fn(res); ferr != nil {
					return ferr
				}
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// batchContent streams a file: an input file as it was uploaded, or a
// job's results as OpenAI batch output or error lines.
func (p *Provider) batchContent(ctx context.Context, id string) (*http.Response, error) {
	if m := batchFileID.FindStringSubmatch(id); m != nil {
		bucket, prefix := p.batchFolder(m[1])
		resp, err := p.gcs(ctx, http.MethodGet, p.objectURL(bucket, prefix+"source.jsonl")+"?alt=media", nil)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusNotFound {
				return provider.ErrorReply(http.StatusNotFound, "not_found", "no such file"), nil
			}
			return gcsFailure(resp), nil
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/jsonl"}}, Body: resp.Body}, nil
	}
	m := batchOutID.FindStringSubmatch(id)
	if m == nil {
		return provider.ErrorReply(http.StatusNotFound, "not_found", "no such file"), nil
	}
	failures, jobID, fid := m[1] == "err", m[2], m[3]
	j, failure, err := p.job(ctx, jobID)
	if err != nil || failure != nil {
		return failure, err
	}
	dir := p.outputDir(j, fid)
	if dir == "" {
		return provider.ErrorReply(http.StatusNotFound, "not_found", "the job wrote no results"), nil
	}
	bucket, prefix := p.batchFolder(fid)
	data, found, failure, err := p.gcsRead(ctx, bucket, prefix+"source.jsonl")
	if err != nil || failure != nil {
		return failure, err
	}
	if !found {
		return provider.ErrorReply(http.StatusNotFound, "not_found", "the batch's input file was deleted, and its results cannot be read without it"), nil
	}
	src := provider.ParseBatchSource(data)
	// The results are translated as they are read, so a large file is
	// not held in memory.
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	pr, pw := io.Pipe()
	go func() {
		failure, err := p.results(ctx, dir, func(r result) error {
			customID, model := src.Row(r.row)
			var out []byte
			switch {
			case r.failure != "" && failures:
				out = provider.BatchFailure(r.row, customID, "request_failed", r.failure)
			case r.failure == "" && !failures:
				chat, _, _, err := gemini.GenerateToChat(r.response, model)
				if err != nil {
					return nil
				}
				out = provider.BatchResult(r.row, customID, chat)
			default:
				return nil
			}
			_, err := pw.Write(out)
			return err
		})
		if err == nil && failure != nil {
			_ = failure.Body.Close()
			err = io.ErrUnexpectedEOF
		}
		pw.CloseWithError(err)
	}()
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/jsonl"}},
		Body: &cancelOnClose{ReadCloser: pr, cancel: cancel}}, nil
}

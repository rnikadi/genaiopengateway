package bedrock

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/provider"
	"github.com/rnikadi/genaiopengateway/internal/usage"
)

// OpenAI's Batch API on Bedrock batch inference (docs/batch.md). An
// uploaded file becomes three objects below <s3_output_uri>/batches/
// <file id>/: source.jsonl as the client sent it, input.jsonl with each
// request translated to Converse, and meta.json. A batch is a model
// invocation job of type Converse on input.jsonl, which writes below
// out/<job id>/. The ids the gateway hands out name the file and the
// job, so nothing else is stored.

var (
	batchFileID = regexp.MustCompile(`^file_br_([0-9a-f]{24})$`)
	batchOutID  = regexp.MustCompile(`^file_br(out|err)_([a-z0-9]{12})_([0-9a-f]{24})$`)
	batchJobID  = regexp.MustCompile(`^batch_br_([a-z0-9]{12})_([0-9a-f]{24})$`)
)

// maxBatchFile bounds a batch file read into memory.
const maxBatchFile = 512 << 20

// batchTotals caches what finished jobs used, by job id.
var batchTotals sync.Map

// batches reports whether the provider is configured for batch jobs.
func (p *Provider) batches() bool { return p.cfg.S3OutputURI != "" && p.cfg.BatchRoleARN != "" }

// batchFolder returns the bucket and key prefix of a file's folder.
func (p *Provider) batchFolder(fid string) (bucket, prefix string) {
	bucket, prefix, _ = strings.Cut(strings.TrimPrefix(p.cfg.S3OutputURI, "s3://"), "/")
	return bucket, strings.TrimPrefix(strings.Trim(prefix, "/")+"/batches/"+fid+"/", "/")
}

// s3 calls S3 on one object with AWS credentials from the default
// chain. The caller closes the response body.
func (p *Provider) s3(ctx context.Context, method, bucket, key string, body []byte) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, p.cfg.Timeout.Std())
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.s3URL(bucket, (&url.URL{Path: key}).EscapedPath()), rd)
	if err != nil {
		cancel()
		return nil, err
	}
	if err := p.auth.signS3(req, body); err != nil {
		cancel()
		return provider.ErrorReply(http.StatusNotImplemented, "api_error", "batches on Bedrock need AWS credentials from the default chain for S3: "+err.Error()), nil
	}
	resp, err := p.client.Do(req)
	if err != nil {
		cancel()
		return nil, err
	}
	resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

// s3Read reads a whole object. A missing object is found false.
func (p *Provider) s3Read(ctx context.Context, bucket, key string) (data []byte, found bool, failure *http.Response, err error) {
	resp, err := p.s3(ctx, http.MethodGet, bucket, key, nil)
	if err != nil {
		return nil, false, nil, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, false, nil, nil
	case resp.StatusCode != http.StatusOK:
		return nil, false, s3Failure(resp), nil
	}
	data, err = io.ReadAll(io.LimitReader(resp.Body, maxBatchFile))
	return data, err == nil, nil, err
}

// s3Failure is an S3 error as a gateway answer.
func s3Failure(resp *http.Response) *http.Response {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	status := http.StatusBadGateway
	if resp.StatusCode == http.StatusNotImplemented {
		status = resp.StatusCode
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(b, &e) == nil && e.Error.Message != "" {
			return provider.ErrorReply(status, "api_error", e.Error.Message)
		}
	}
	return provider.ErrorReply(status, "api_error", "S3 answered "+resp.Status+": "+strings.TrimSpace(string(b)))
}

// forwardBatch serves the files and batches calls of provider.Forwarder.
func (p *Provider) forwardBatch(ctx context.Context, method, path string, body io.Reader, contentType string) (*http.Response, error) {
	if !p.batches() {
		return provider.ErrorReply(http.StatusBadRequest, "invalid_request_error", "this Bedrock provider has no s3_output_uri and batch_role_arn, which batches need"), nil
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
	return provider.ErrorReply(http.StatusNotFound, "not_found", "Bedrock batches do not serve "+method+" "+path), nil
}

// batchUpload stores an input file and its Converse translation.
func (p *Provider) batchUpload(ctx context.Context, body io.Reader, contentType string) (*http.Response, error) {
	up, err := provider.ReadBatchUpload(body, contentType, maxBatchFile)
	if err != nil {
		return provider.ErrorReply(http.StatusBadRequest, "invalid_request_error", err.Error()), nil
	}
	var input bytes.Buffer
	for i, l := range up.Lines {
		var cr chatRequest
		if err := json.Unmarshal(l.Body, &cr); err != nil {
			return provider.ErrorReply(http.StatusBadRequest, "invalid_request_error", "request "+l.CustomID+" is not a valid chat completion request"), nil
		}
		conv, err := translateRequest(&cr)
		if err != nil {
			return provider.ErrorReply(http.StatusBadRequest, "invalid_request_error", "request "+l.CustomID+": "+err.Error()), nil
		}
		rec, err := json.Marshal(map[string]any{"recordId": provider.BatchRow(i), "modelInput": conv})
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
		key  string
		data []byte
	}{{"source.jsonl", up.Data}, {"input.jsonl", input.Bytes()}, {"meta.json", mb}} {
		resp, err := p.s3(ctx, http.MethodPut, bucket, prefix+o.key, o.data)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			defer resp.Body.Close()
			return s3Failure(resp), nil
		}
		_ = resp.Body.Close()
	}
	return provider.JSONReply(http.StatusOK, provider.FileObject("file_br_"+fid, "batch", meta)), nil
}

// batchMeta reads an input file's description.
func (p *Provider) batchMeta(ctx context.Context, fid string) (*provider.BatchFileMeta, *http.Response, error) {
	bucket, prefix := p.batchFolder(fid)
	data, found, failure, err := p.s3Read(ctx, bucket, prefix+"meta.json")
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

// batchDelete removes an input file from S3. Result files stay in the
// bucket: the output and error files of a job are one object there.
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
	for _, key := range []string{"meta.json", "input.jsonl", "source.jsonl"} {
		resp, err := p.s3(ctx, http.MethodDelete, bucket, prefix+key, nil)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotFound {
			defer resp.Body.Close()
			return s3Failure(resp), nil
		}
		_ = resp.Body.Close()
	}
	return provider.JSONReply(http.StatusOK, deleted), nil
}

// batchCreate starts a model invocation job on an input file.
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
	folder := "s3://" + bucket + "/" + prefix
	token := provider.BatchFileID()
	job, _ := json.Marshal(map[string]any{
		"jobName":             "gog-" + fid[:12] + "-" + token[:8],
		"clientRequestToken":  token,
		"roleArn":             p.cfg.BatchRoleARN,
		"modelId":             meta.Model,
		"modelInvocationType": "Converse",
		"inputDataConfig":     map[string]any{"s3InputDataConfig": map[string]string{"s3InputFormat": "JSONL", "s3Uri": folder + "input.jsonl"}},
		"outputDataConfig":    map[string]any{"s3OutputDataConfig": map[string]string{"s3Uri": folder + "out/"}},
	})
	resp, err := p.call(ctx, http.MethodPost, p.control+"/model-invocation-job", job)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		e := openAIError(resp)
		return &http.Response{StatusCode: e.StatusCode, Header: e.Header, Body: e.Body}, nil
	}
	var created struct {
		JobARN string `json:"jobArn"`
	}
	buf, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = json.Unmarshal(buf, &created)
	jobID := created.JobARN[strings.LastIndex(created.JobARN, "/")+1:]
	id := "batch_br_" + jobID + "_" + fid
	if !batchJobID.MatchString(id) {
		return provider.ErrorReply(http.StatusBadGateway, "api_error", "the upstream answer names no job"), nil
	}
	return provider.JSONReply(http.StatusOK, provider.BatchState{ID: id, InputFileID: req.InputFileID, Status: "validating", Total: meta.Requests}.Object()), nil
}

// batchStatus maps a model invocation job status to OpenAI's.
var batchStatus = map[string]string{
	"Submitted": "validating", "Validating": "validating", "Scheduled": "in_progress", "InProgress": "in_progress",
	"Completed": "completed", "PartiallyCompleted": "completed", "Failed": "failed",
	"Stopping": "cancelling", "Stopped": "cancelled", "Expired": "expired",
}

// awsTime reads a timestamp Bedrock wrote as RFC 3339 or as seconds.
func awsTime(raw json.RawMessage) int64 {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		t, _ := time.Parse(time.RFC3339, s)
		if t.IsZero() {
			return 0
		}
		return t.Unix()
	}
	var f float64
	_ = json.Unmarshal(raw, &f)
	return int64(f)
}

// batchGet retrieves a batch, stopping its job first when cancel is set.
func (p *Provider) batchGet(ctx context.Context, id string, cancel bool) (*http.Response, error) {
	m := batchJobID.FindStringSubmatch(id)
	if m == nil {
		return provider.ErrorReply(http.StatusNotFound, "not_found", "no such batch"), nil
	}
	jobID, fid := m[1], m[2]
	if cancel {
		resp, err := p.call(ctx, http.MethodPost, p.control+"/model-invocation-job/"+jobID+"/stop", []byte("{}"))
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			defer resp.Body.Close()
			e := openAIError(resp)
			return &http.Response{StatusCode: e.StatusCode, Header: e.Header, Body: e.Body}, nil
		}
		_ = resp.Body.Close()
	}
	resp, err := p.call(ctx, http.MethodGet, p.control+"/model-invocation-job/"+jobID, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		e := openAIError(resp)
		return &http.Response{StatusCode: e.StatusCode, Header: e.Header, Body: e.Body}, nil
	}
	var job struct {
		Status     string          `json:"status"`
		Message    string          `json:"message"`
		SubmitTime json.RawMessage `json:"submitTime"`
		Total      int64           `json:"totalRecordCount"`
		Success    int64           `json:"successRecordCount"`
		Errors     int64           `json:"errorRecordCount"`
	}
	buf, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if json.Unmarshal(buf, &job) != nil || batchStatus[job.Status] == "" {
		return provider.ErrorReply(http.StatusBadGateway, "api_error", "the upstream answer is not a model invocation job"), nil
	}
	st := provider.BatchState{
		ID: id, InputFileID: "file_br_" + fid, Status: batchStatus[job.Status], Created: awsTime(job.SubmitTime),
		Total: job.Total, Completed: job.Success, Failed: job.Errors,
	}
	if st.Status == "failed" {
		st.Message = job.Message
	}
	// A stopped or expired job may have finished some records, which
	// AWS bills, so its manifest is read too.
	if st.Status == "completed" || st.Status == "cancelled" || st.Status == "expired" {
		man, failure, err := p.batchManifest(ctx, jobID, fid)
		if err != nil || failure != nil {
			return failure, err
		}
		if man != nil {
			st.Total, st.Completed, st.Failed = man.Total, man.Success, man.Errors
			st.Tokens = &usage.Tokens{InputTokens: man.Input, OutputTokens: man.Output, Reported: true}
			if man.Success > 0 {
				st.OutputFileID = "file_brout_" + jobID + "_" + fid
			}
			if man.Errors > 0 {
				st.ErrorFileID = "file_brerr_" + jobID + "_" + fid
			}
		}
	}
	return provider.JSONReply(http.StatusOK, st.Object()), nil
}

// manifest is manifest.json.out, which Bedrock writes when a job ends.
type manifest struct {
	Total   int64 `json:"totalRecordCount"`
	Success int64 `json:"successRecordCount"`
	Errors  int64 `json:"errorRecordCount"`
	Input   int64 `json:"inputTokenCount"`
	Output  int64 `json:"outputTokenCount"`
}

// batchManifest reads a job's manifest, or nil when it wrote none.
func (p *Provider) batchManifest(ctx context.Context, jobID, fid string) (*manifest, *http.Response, error) {
	if v, ok := batchTotals.Load(p.cfg.S3OutputURI + "|" + jobID); ok {
		return v.(*manifest), nil, nil
	}
	bucket, prefix := p.batchFolder(fid)
	data, found, failure, err := p.s3Read(ctx, bucket, prefix+"out/"+jobID+"/manifest.json.out")
	if err != nil || failure != nil || !found {
		return nil, failure, err
	}
	var man manifest
	if json.Unmarshal(data, &man) != nil {
		return nil, nil, nil
	}
	batchTotals.Store(p.cfg.S3OutputURI+"|"+jobID, &man)
	return &man, nil, nil
}

// batchContent streams a file: an input file as it was uploaded, or a
// job's results as OpenAI batch output or error lines.
func (p *Provider) batchContent(ctx context.Context, id string) (*http.Response, error) {
	if m := batchFileID.FindStringSubmatch(id); m != nil {
		bucket, prefix := p.batchFolder(m[1])
		resp, err := p.s3(ctx, http.MethodGet, bucket, prefix+"source.jsonl", nil)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusNotFound {
				return provider.ErrorReply(http.StatusNotFound, "not_found", "no such file"), nil
			}
			return s3Failure(resp), nil
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/jsonl"}}, Body: resp.Body}, nil
	}
	m := batchOutID.FindStringSubmatch(id)
	if m == nil {
		return provider.ErrorReply(http.StatusNotFound, "not_found", "no such file"), nil
	}
	failures, jobID, fid := m[1] == "err", m[2], m[3]
	bucket, prefix := p.batchFolder(fid)
	data, found, failure, err := p.s3Read(ctx, bucket, prefix+"source.jsonl")
	if err != nil || failure != nil {
		return failure, err
	}
	if !found {
		return provider.ErrorReply(http.StatusNotFound, "not_found", "the batch's input file was deleted, and its results cannot be read without it"), nil
	}
	src := provider.ParseBatchSource(data)
	resp, err := p.s3(ctx, http.MethodGet, bucket, prefix+"out/"+jobID+"/input.jsonl.out", nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			return provider.ErrorReply(http.StatusNotFound, "not_found", "the job wrote no results"), nil
		}
		return s3Failure(resp), nil
	}
	body := pipe(resp.Body, func(w io.Writer) error {
		rd := bufio.NewReaderSize(resp.Body, 64<<10)
		for {
			line, err := rd.ReadBytes('\n')
			if out := batchRecord(line, src, failures); out != nil {
				if _, werr := w.Write(out); werr != nil {
					return werr
				}
			}
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
		}
	})
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/jsonl"}}, Body: body}, nil
}

// batchRecord translates one record of a job's output: a line of the
// output file, or of the error file when failures is set. Records of
// the other file give nil.
func batchRecord(line []byte, src *provider.BatchSource, failures bool) []byte {
	var rec struct {
		RecordID string            `json:"recordId"`
		Output   *converseResponse `json:"modelOutput"`
		Error    *struct {
			Code    json.RawMessage `json:"errorCode"`
			Message string          `json:"errorMessage"`
		} `json:"error"`
	}
	if len(bytes.TrimSpace(line)) == 0 || json.Unmarshal(line, &rec) != nil {
		return nil
	}
	customID, model := src.Row(rec.RecordID)
	switch {
	case rec.Error != nil && failures:
		return provider.BatchFailure(rec.RecordID, customID, strings.Trim(string(rec.Error.Code), `"`), rec.Error.Message)
	case rec.Error == nil && rec.Output != nil && !failures:
		return provider.BatchResult(rec.RecordID, customID, translateResponse(rec.Output, model))
	}
	return nil
}

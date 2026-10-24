package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/auth"
	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/provider"
	"github.com/rnikadi/genaiopengateway/internal/store"
)

// maxJobObject bounds the job object kept from a create response.
const maxJobObject = 1 << 20

var (
	errJobNotFound = errors.New("video not found")
	errNoDatabase  = errors.New("video jobs need a database")
)

// createsJob reports whether an endpoint answers with a new video job.
func createsJob(ep provider.Endpoint) bool {
	return ep == provider.Videos || ep == provider.VideoRemix
}

// ownsJob reports whether a caller owns a job: same organization and
// team. Static keys share a team, so each owns only its own jobs.
func ownsJob(j *store.VideoJob, p *auth.Principal) bool {
	return j.OrgID == p.OrgID && j.TeamID == p.TeamID && (p.TeamID != auth.StaticTeamID || j.KeyID == p.KeyID)
}

// ownedJob loads the job named in the path, if the caller owns it and
// may use its model. Other callers learn nothing about it.
func (s *Server) ownedJob(r *http.Request, p *auth.Principal) (*store.VideoJob, error) {
	if s.store == nil {
		return nil, errNoDatabase
	}
	j, err := s.store.GetVideoJob(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) || (err == nil && (!ownsJob(j, p) || !p.AllowsModel(j.Model))) {
		return nil, errJobNotFound
	}
	return j, err
}

// jobError is the status, code and message for an ownedJob error.
func jobError(err error) (int, string, string) {
	switch {
	case errors.Is(err, errJobNotFound):
		return http.StatusNotFound, codeNotFound, err.Error()
	case errors.Is(err, errNoDatabase):
		return http.StatusNotImplemented, codeNoDatabase, err.Error()
	}
	return http.StatusInternalServerError, codeInvalidRequest, "the video job could not be read"
}

func writeJobError(w http.ResponseWriter, err error) {
	status, code, msg := jobError(err)
	writeError(w, provider.Videos, status, code, msg)
}

// recordJob stores a job the upstream just created, so later calls reach
// the same target and only its owner.
func (s *Server) recordJob(ctx context.Context, p *auth.Principal, model string, t config.Target, body *captureBuffer) {
	var obj struct {
		ID string `json:"id"`
	}
	if body.truncated || json.Unmarshal(body.Bytes(), &obj) != nil || obj.ID == "" {
		s.log.Error("video job response has no id", "provider", t.Provider)
		return
	}
	err := s.store.CreateVideoJob(ctx, &store.VideoJob{
		ID: obj.ID, OrgID: p.OrgID, TeamID: p.TeamID, KeyID: p.KeyID,
		Model: model, Provider: t.Provider, UpstreamModel: t.Model, Object: json.RawMessage(body.Bytes()),
	})
	if err != nil {
		s.log.Error("recording video job failed", "video_id", obj.ID, "err", err)
	}
}

// videoCaller authenticates a call on video jobs.
func (s *Server) videoCaller(w http.ResponseWriter, r *http.Request) (*auth.Principal, bool) {
	p, ok := principalFrom(r.Context())
	if !ok {
		p, ok = s.authenticate(r)
	}
	if !ok {
		writeError(w, provider.Videos, http.StatusUnauthorized, codeInvalidKey, "missing or invalid API key")
		return nil, false
	}
	if s.store == nil {
		writeJobError(w, errNoDatabase)
		return nil, false
	}
	return p, true
}

// videoJob serves retrieve, delete and content on a job, at the target
// that created it. These calls are not charged: clients poll them.
func (s *Server) videoJob(method, suffix string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		p, ok := s.videoCaller(w, r)
		if !ok {
			return
		}
		j, err := s.ownedJob(r, p)
		if err != nil {
			writeJobError(w, err)
			return
		}
		f, ok := s.rt.Load().router.Forwarder(j.Provider)
		if !ok {
			writeError(w, provider.Videos, http.StatusNotFound, codeModelNotFound, "the provider that made this video is no longer configured")
			return
		}
		path := "/videos/" + url.PathEscape(j.ID) + suffix
		if v := r.URL.Query().Get("variant"); suffix != "" && v != "" {
			path += "?" + url.Values{"variant": {v}}.Encode()
		}
		resp, err := f.Forward(r.Context(), method, path)
		if err != nil {
			s.log.Warn("video job call failed", "video_id", j.ID, "provider", j.Provider, "err", err)
			writeError(w, provider.Videos, http.StatusBadGateway, codeAllTargetsFailed, "the upstream did not answer")
			return
		}
		defer resp.Body.Close()
		for _, h := range []string{"Content-Type", "Content-Length", "Content-Disposition"} {
			if v := resp.Header.Get(h); v != "" {
				w.Header().Set(h, v)
			}
		}
		w.Header().Set("X-Gateway-Target", j.Provider+"/"+j.UpstreamModel)
		ctx := context.WithoutCancel(r.Context())
		switch {
		case resp.StatusCode != http.StatusOK || suffix != "":
			w.WriteHeader(resp.StatusCode)
			copyFlush(w, resp.Body)
		case method == http.MethodDelete:
			w.WriteHeader(resp.StatusCode)
			copyFlush(w, resp.Body)
			if err := s.store.DeleteVideoJob(ctx, j.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
				s.log.Error("deleting video job failed", "video_id", j.ID, "err", err)
			}
		default:
			// A retrieve refreshes the stored object that listing shows.
			obj, err := io.ReadAll(io.LimitReader(resp.Body, maxJobObject+1))
			if err != nil {
				writeError(w, provider.Videos, http.StatusBadGateway, codeAllTargetsFailed, "reading the upstream response failed")
				return
			}
			w.WriteHeader(resp.StatusCode)
			_, _ = w.Write(obj)
			if len(obj) <= maxJobObject && json.Valid(obj) {
				if err := s.store.UpdateVideoJob(ctx, j.ID, obj); err != nil && !errors.Is(err, store.ErrNotFound) {
					s.log.Error("updating video job failed", "video_id", j.ID, "err", err)
				}
			}
		}
		s.log.Info("video job", "video_id", j.ID, "method", method, "path", r.URL.Path, "org_id", p.OrgID, "team_id", p.TeamID,
			"key_id", p.KeyID, "target", j.Provider+"/"+j.UpstreamModel, "status", resp.StatusCode, "latency_ms", time.Since(start).Milliseconds())
	}
}

// listVideos lists the caller's jobs, newest first, as last stored.
func (s *Server) listVideos(w http.ResponseWriter, r *http.Request) {
	p, ok := s.videoCaller(w, r)
	if !ok {
		return
	}
	n := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		var err error
		if n, err = strconv.Atoi(v); err != nil || n < 1 || n > 100 {
			writeError(w, provider.Videos, http.StatusBadRequest, codeInvalidRequest, "limit must be between 1 and 100")
			return
		}
	}
	keyID := ""
	if p.TeamID == auth.StaticTeamID {
		keyID = p.KeyID
	}
	jobs, more, err := s.store.ListVideoJobs(r.Context(), p.OrgID, p.TeamID, keyID, store.Page{Limit: n, After: r.URL.Query().Get("after")})
	if err != nil {
		s.log.Error("listing video jobs failed", "err", err)
		writeError(w, provider.Videos, http.StatusInternalServerError, codeInvalidRequest, "video jobs could not be listed")
		return
	}
	data := []json.RawMessage{}
	for _, j := range jobs {
		if p.AllowsModel(j.Model) {
			data = append(data, j.Object)
		}
	}
	out := map[string]any{"object": "list", "data": data, "has_more": more}
	if len(jobs) > 0 {
		out["first_id"], out["last_id"] = jobs[0].ID, jobs[len(jobs)-1].ID
	}
	writeJSON(w, http.StatusOK, out)
}

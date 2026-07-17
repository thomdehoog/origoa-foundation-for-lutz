// Package httpapi exposes the Foundation as a REST API.
package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/thomdehoog/origoa/internal/core"
	"github.com/thomdehoog/origoa/internal/ojson"
)

const maxBody = 4 << 20 // 4 MiB request bodies

type Server struct {
	f *core.Foundation
}

func New(f *core.Foundation) http.Handler {
	s := &Server{f: f}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/tree", s.tree)
	mux.HandleFunc("GET /api/search", s.search)
	mux.HandleFunc("POST /api/admin/reindex", s.reindex)

	for _, r := range []struct{ route, kind string }{
		{"entries", core.KindEntry}, {"documents", core.KindDocument},
	} {
		kind := r.kind
		mux.HandleFunc("POST /api/"+r.route, func(w http.ResponseWriter, req *http.Request) { s.create(w, req, kind) })
	}
	mux.HandleFunc("GET /api/entries/{guid}", s.get)
	mux.HandleFunc("GET /api/documents/{guid}", s.get)
	mux.HandleFunc("GET /api/links/{guid}", s.get)
	mux.HandleFunc("GET /api/comments/{guid}", s.get)
	mux.HandleFunc("PUT /api/entries/{guid}", s.update)
	mux.HandleFunc("PUT /api/documents/{guid}", s.update)
	for _, route := range []string{"entries", "documents", "links", "comments"} {
		mux.HandleFunc("DELETE /api/"+route+"/{guid}", s.delete)
	}

	mux.HandleFunc("POST /api/links", s.createLink)
	mux.HandleFunc("POST /api/comments", s.createComment)

	mux.HandleFunc("GET /api/artifacts/{guid}/links", s.artifactLinks)
	mux.HandleFunc("GET /api/artifacts/{guid}/comments", s.artifactComments)
	mux.HandleFunc("GET /api/artifacts/{guid}/history", s.history)
	mux.HandleFunc("POST /api/artifacts/{guid}/move", s.move)
	mux.HandleFunc("POST /api/artifacts/{guid}/transition", s.transition)

	mux.HandleFunc("GET /api/schemas", s.schemas)
	mux.HandleFunc("GET /api/schemas/effective", s.effectiveSchema)
	mux.HandleFunc("PUT /api/schemas/{name}", s.putSchema)
	mux.HandleFunc("GET /api/workflows/{id}", s.workflow)
	mux.HandleFunc("PUT /api/workflows/{name}", s.putWorkflow)

	return mux
}

// ---- plumbing ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, core.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, core.ErrConflict):
		status = http.StatusConflict
	case errors.Is(err, core.ErrValidation):
		status = http.StatusBadRequest
	}
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func readBody(w http.ResponseWriter, req *http.Request, v any) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, maxBody))
	if err == nil {
		err = json.Unmarshal(body, v)
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body: " + err.Error()})
		return false
	}
	return true
}

// ---- artifacts ----

func (s *Server) tree(w http.ResponseWriter, req *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"rev":       s.f.Head(),
		"folders":   s.f.Folders(),
		"artifacts": s.f.List(req.URL.Query().Get("kind"), req.URL.Query().Get("type"), "", true),
	})
}

func (s *Server) search(w http.ResponseWriter, req *http.Request) {
	q := req.URL.Query()
	writeJSON(w, http.StatusOK, map[string]any{
		"artifacts": s.f.Search(q.Get("q"), q.Get("kind"), q.Get("type")),
	})
}

func (s *Server) reindex(w http.ResponseWriter, _ *http.Request) {
	if err := s.f.Reindex(); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"rev": s.f.Head()})
}

func (s *Server) create(w http.ResponseWriter, req *http.Request, kind string) {
	body := ojson.New()
	if !readBody(w, req, body) {
		return
	}
	meta, err := s.f.CreateArtifact(kind, body.GetString("path"), body.GetString("type"), body)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"meta": meta})
}

func (s *Server) get(w http.ResponseWriter, req *http.Request) {
	meta, obj, err := s.f.Artifact(req.PathValue("guid"))
	if err != nil {
		writeErr(w, err)
		return
	}
	res := map[string]any{"meta": meta, "data": obj}
	if req.URL.Query().Get("resolve") == "1" && meta.Kind == core.KindEntry {
		fields, chain, err := s.f.ResolveOverlay(meta.GUID)
		if err != nil {
			writeErr(w, err)
			return
		}
		res["resolved"] = map[string]any{"fields": fields, "chain": chain}
	}
	w.Header().Set("ETag", meta.ETag)
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) update(w http.ResponseWriter, req *http.Request) {
	patch := ojson.New()
	if !readBody(w, req, patch) {
		return
	}
	meta, err := s.f.UpdateArtifact(req.PathValue("guid"), patch, req.Header.Get("If-Match"))
	if err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("ETag", meta.ETag)
	writeJSON(w, http.StatusOK, map[string]any{"meta": meta})
}

func (s *Server) delete(w http.ResponseWriter, req *http.Request) {
	if err := s.f.DeleteArtifact(req.PathValue("guid")); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) move(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Path string `json:"path"`
	}
	if !readBody(w, req, &body) {
		return
	}
	meta, err := s.f.MoveArtifact(req.PathValue("guid"), body.Path)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"meta": meta})
}

// ---- links & comments ----

func (s *Server) createLink(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Type   string          `json:"type"`
		Source string          `json:"source"`
		Target string          `json:"target"`
		Fields json.RawMessage `json:"fields"`
	}
	if !readBody(w, req, &body) {
		return
	}
	meta, err := s.f.CreateLink(body.Type, body.Source, body.Target, body.Fields)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"meta": meta})
}

func (s *Server) createComment(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Subject string `json:"subject"`
		Text    string `json:"text"`
		Parent  string `json:"parent"`
		Author  string `json:"author"`
	}
	if !readBody(w, req, &body) {
		return
	}
	meta, err := s.f.CreateComment(body.Subject, body.Text, body.Parent, body.Author)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"meta": meta})
}

func (s *Server) artifactLinks(w http.ResponseWriter, req *http.Request) {
	guid := req.PathValue("guid")
	if _, err := s.f.Meta(guid); err != nil {
		writeErr(w, err)
		return
	}
	in, out := s.f.Links(guid)
	writeJSON(w, http.StatusOK, map[string]any{"incoming": in, "outgoing": out})
}

func (s *Server) artifactComments(w http.ResponseWriter, req *http.Request) {
	guid := req.PathValue("guid")
	if _, err := s.f.Meta(guid); err != nil {
		writeErr(w, err)
		return
	}
	comments := []any{}
	for _, m := range s.f.Comments(guid) {
		if _, obj, err := s.f.Artifact(m.GUID); err == nil {
			comments = append(comments, obj)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"comments": comments})
}

func (s *Server) history(w http.ResponseWriter, req *http.Request) {
	limit, _ := strconv.Atoi(req.URL.Query().Get("limit"))
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	log, err := s.f.History(req.PathValue("guid"), limit)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"history": log})
}

func (s *Server) transition(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Workflow string `json:"workflow"`
		To       string `json:"to"`
	}
	if !readBody(w, req, &body) {
		return
	}
	meta, err := s.f.Transition(req.PathValue("guid"), body.Workflow, body.To)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"meta": meta})
}

// ---- configuration ----

func (s *Server) schemas(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"schemas": s.f.Schemas()})
}

func (s *Server) effectiveSchema(w http.ResponseWriter, req *http.Request) {
	q := req.URL.Query()
	schema, err := s.f.EffectiveSchema(q.Get("type"), q.Get("path"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"schema": schema})
}

func (s *Server) putSchema(w http.ResponseWriter, req *http.Request) {
	var schema core.Schema
	if !readBody(w, req, &schema) {
		return
	}
	if err := s.f.PutSchema(req.URL.Query().Get("scope"), req.PathValue("name"), &schema); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"schema": schema})
}

func (s *Server) workflow(w http.ResponseWriter, req *http.Request) {
	wf, err := s.f.WorkflowDef(req.PathValue("id"), req.URL.Query().Get("path"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"workflow": wf})
}

func (s *Server) putWorkflow(w http.ResponseWriter, req *http.Request) {
	var wf core.Workflow
	if !readBody(w, req, &wf) {
		return
	}
	if err := s.f.PutWorkflow(req.URL.Query().Get("scope"), req.PathValue("name"), &wf); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"workflow": wf})
}

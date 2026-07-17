package httpapi

import (
	"context"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/thomdehoog/origoa-foundation/internal/repository"
)

const maxBody = 1 << 20

//go:embed web/*
var assets embed.FS

type Server struct {
	repository *repository.Repository
	handler    http.Handler
}

func New(repo *repository.Repository) *Server {
	server := &Server{repository: repo}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", server.health)
	mux.HandleFunc("GET /api/artifacts", server.listArtifacts)
	mux.HandleFunc("POST /api/artifacts", server.createArtifact)
	mux.HandleFunc("GET /api/artifacts/{guid}", server.getArtifact)
	mux.HandleFunc("PUT /api/artifacts/{guid}", server.updateArtifact)
	mux.HandleFunc("DELETE /api/artifacts/{guid}", server.deleteArtifact)
	mux.HandleFunc("GET /api/artifacts/{guid}/schema", server.schema)
	mux.HandleFunc("GET /api/artifacts/{guid}/overlay", server.overlay)
	mux.HandleFunc("GET /api/artifacts/{guid}/links", server.links)
	mux.HandleFunc("GET /api/artifacts/{guid}/workflows", server.workflows)
	mux.HandleFunc("POST /api/artifacts/{guid}/transitions", server.transition)
	mux.HandleFunc("GET /api/artifacts/{guid}/history", server.history)
	mux.HandleFunc("GET /api/search", server.search)
	mux.HandleFunc("GET /api/repository/tree", server.tree)
	mux.Handle("GET /", server.static())
	server.handler = server.middleware(mux)
	return server
}

func (s *Server) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	s.handler.ServeHTTP(response, request)
}

func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		started := time.Now()
		response.Header().Set("Content-Security-Policy", "default-src 'self'; base-uri 'none'; connect-src 'self'; form-action 'self'; frame-ancestors 'none'; img-src 'self' data:; object-src 'none'; script-src 'self'; style-src 'self'")
		response.Header().Set("Referrer-Policy", "no-referrer")
		response.Header().Set("X-Content-Type-Options", "nosniff")
		response.Header().Set("X-Frame-Options", "DENY")
		response.Header().Set("Permissions-Policy", "camera=(), geolocation=(), microphone=()")
		defer func() {
			if recovered := recover(); recovered != nil {
				slog.Error("request panic", "error", recovered)
				writeError(response, request, fmt.Errorf("panic: %v", recovered))
			}
			slog.Debug("request", "method", request.Method, "path", request.URL.Path, "duration", time.Since(started))
		}()
		next.ServeHTTP(response, request)
	})
}

func (s *Server) static() http.Handler {
	web, err := fs.Sub(assets, "web")
	if err != nil {
		panic(err)
	}
	files := http.FileServerFS(web)
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if dot := strings.LastIndex(request.URL.Path, "."); dot >= 0 {
			if contentType := mime.TypeByExtension(request.URL.Path[dot:]); contentType != "" {
				response.Header().Set("Content-Type", contentType)
			}
		}
		response.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(response, request)
	})
}

func writeJSON(response http.ResponseWriter, status int, value any) {
	response.Header().Set("Content-Type", "application/json; charset=utf-8")
	response.WriteHeader(status)
	if err := json.NewEncoder(response).Encode(value); err != nil {
		slog.Error("encode response", "error", err)
	}
}

func writeError(response http.ResponseWriter, request *http.Request, err error) {
	var repoError *repository.Error
	if errors.As(err, &repoError) {
		writeJSON(response, repoError.Status, map[string]any{"error": map[string]string{
			"code": repoError.Code, "message": repoError.Message,
		}})
		return
	}
	slog.Error("request failed", "method", request.Method, "path", request.URL.Path, "error", err)
	writeJSON(response, http.StatusInternalServerError, map[string]any{"error": map[string]string{
		"code": "internal_error", "message": "Internal server error.",
	}})
}

func decodeJSON(response http.ResponseWriter, request *http.Request, destination any, strict bool) error {
	contentType := strings.ToLower(strings.TrimSpace(strings.Split(request.Header.Get("Content-Type"), ";")[0]))
	if contentType != "application/json" {
		return repositoryError(415, "unsupported_media_type", "Content-Type must be application/json.")
	}
	request.Body = http.MaxBytesReader(response, request.Body, maxBody)
	decoder := json.NewDecoder(request.Body)
	if strict {
		decoder.DisallowUnknownFields()
	}
	if err := decoder.Decode(destination); err != nil {
		if strings.Contains(err.Error(), "request body too large") {
			return repositoryError(413, "body_too_large", "Request body is too large.")
		}
		return repositoryError(400, "invalid_json", "Request body must contain one valid JSON value.")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return repositoryError(400, "invalid_json", "Request body must contain one valid JSON value.")
	}
	return nil
}

func repositoryError(status int, code, message string) error {
	return &repository.Error{Status: status, Code: code, Message: message}
}

func expectedETag(request *http.Request) (string, error) {
	value := strings.TrimSpace(request.Header.Get("If-Match"))
	if value == "" {
		return "", repositoryError(428, "precondition_required", "If-Match is required.")
	}
	if strings.HasPrefix(value, "W/") || strings.Contains(value, ",") || value == "*" || len(value) < 2 || value[0] != '"' || value[len(value)-1] != '"' {
		return "", repositoryError(400, "invalid_etag", "If-Match must contain one strong ETag.")
	}
	value = value[1 : len(value)-1]
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(decoded) != 32 {
		return "", repositoryError(400, "invalid_etag", "If-Match is invalid.")
	}
	return value, nil
}

func setETag(response http.ResponseWriter, value string) {
	response.Header().Set("ETag", `"`+value+`"`)
}

func parseLimit(request *http.Request) (int, error) {
	value := request.URL.Query().Get("limit")
	if value == "" {
		return 0, nil
	}
	limit, err := strconv.Atoi(value)
	if err != nil {
		return 0, repositoryError(400, "invalid_limit", "Limit must be a number.")
	}
	return limit, nil
}

func (s *Server) health(response http.ResponseWriter, request *http.Request) {
	revision, err := s.repository.Revision(request.Context())
	if err != nil {
		writeError(response, request, err)
		return
	}
	writeJSON(response, 200, map[string]string{"status": "ok", "revision": revision})
}

func (s *Server) listArtifacts(response http.ResponseWriter, request *http.Request) {
	items, err := s.repository.List(repository.Filters{
		Kind: repository.Kind(request.URL.Query().Get("kind")),
		Type: request.URL.Query().Get("type"),
		Path: request.URL.Query().Get("path"),
	})
	if err != nil {
		writeError(response, request, err)
		return
	}
	writeJSON(response, 200, map[string]any{"items": items, "count": len(items)})
}

func (s *Server) createArtifact(response http.ResponseWriter, request *http.Request) {
	var input repository.CreateInput
	if err := decodeJSON(response, request, &input, true); err != nil {
		writeError(response, request, err)
		return
	}
	item, err := s.repository.Create(request.Context(), input)
	if err != nil {
		writeError(response, request, err)
		return
	}
	setETag(response, item.ETag)
	response.Header().Set("Location", "/api/artifacts/"+item.Artifact.GUID)
	writeJSON(response, 201, item)
}

func (s *Server) getArtifact(response http.ResponseWriter, request *http.Request) {
	item, err := s.repository.Get(request.PathValue("guid"))
	if err != nil {
		writeError(response, request, err)
		return
	}
	setETag(response, item.ETag)
	writeJSON(response, 200, item)
}

func (s *Server) updateArtifact(response http.ResponseWriter, request *http.Request) {
	tag, err := expectedETag(request)
	if err != nil {
		writeError(response, request, err)
		return
	}
	var patch map[string]any
	if err := decodeJSON(response, request, &patch, false); err != nil {
		writeError(response, request, err)
		return
	}
	item, err := s.repository.Update(request.Context(), request.PathValue("guid"), patch, tag)
	if err != nil {
		writeError(response, request, err)
		return
	}
	setETag(response, item.ETag)
	writeJSON(response, 200, item)
}

func (s *Server) deleteArtifact(response http.ResponseWriter, request *http.Request) {
	tag, err := expectedETag(request)
	if err == nil {
		err = s.repository.Delete(request.Context(), request.PathValue("guid"), tag)
	}
	if err != nil {
		writeError(response, request, err)
		return
	}
	response.WriteHeader(http.StatusNoContent)
}

func (s *Server) schema(response http.ResponseWriter, request *http.Request) {
	item, err := s.repository.Get(request.PathValue("guid"))
	if err != nil {
		writeError(response, request, err)
		return
	}
	value, err := s.repository.EffectiveSchema(item.Artifact.Type, item.Path)
	if err != nil {
		writeError(response, request, err)
		return
	}
	writeJSON(response, 200, value)
}

func (s *Server) overlay(response http.ResponseWriter, request *http.Request) {
	value, err := s.repository.ResolveOverlay(request.PathValue("guid"))
	if err != nil {
		writeError(response, request, err)
		return
	}
	writeJSON(response, 200, value)
}

func (s *Server) links(response http.ResponseWriter, request *http.Request) {
	value, err := s.repository.Links(request.PathValue("guid"))
	if err != nil {
		writeError(response, request, err)
		return
	}
	writeJSON(response, 200, value)
}

func (s *Server) workflows(response http.ResponseWriter, request *http.Request) {
	value, err := s.repository.Workflows(request.PathValue("guid"))
	if err != nil {
		writeError(response, request, err)
		return
	}
	writeJSON(response, 200, map[string]any{"items": value})
}

func (s *Server) transition(response http.ResponseWriter, request *http.Request) {
	tag, err := expectedETag(request)
	if err != nil {
		writeError(response, request, err)
		return
	}
	var input struct {
		Workflow   string `json:"workflow"`
		Transition string `json:"transition"`
	}
	if err := decodeJSON(response, request, &input, true); err != nil {
		writeError(response, request, err)
		return
	}
	item, err := s.repository.Transition(request.Context(), request.PathValue("guid"), input.Workflow, input.Transition, tag)
	if err != nil {
		writeError(response, request, err)
		return
	}
	setETag(response, item.ETag)
	writeJSON(response, 200, item)
}

func (s *Server) history(response http.ResponseWriter, request *http.Request) {
	limit, err := parseLimit(request)
	if err != nil {
		writeError(response, request, err)
		return
	}
	items, err := s.repository.History(request.Context(), request.PathValue("guid"), limit)
	if err != nil {
		writeError(response, request, err)
		return
	}
	writeJSON(response, 200, map[string]any{"items": items})
}

func (s *Server) search(response http.ResponseWriter, request *http.Request) {
	limit, err := parseLimit(request)
	if err != nil {
		writeError(response, request, err)
		return
	}
	items, err := s.repository.Search(repository.SearchInput{
		Query: request.URL.Query().Get("q"),
		Kind:  repository.Kind(request.URL.Query().Get("kind")),
		Type:  request.URL.Query().Get("type"),
		Limit: limit,
	})
	if err != nil {
		writeError(response, request, err)
		return
	}
	writeJSON(response, 200, map[string]any{"items": items, "count": len(items)})
}

func (s *Server) tree(response http.ResponseWriter, request *http.Request) {
	value, err := s.repository.Tree()
	if err != nil {
		writeError(response, request, err)
		return
	}
	writeJSON(response, 200, value)
}

func Shutdown(server *http.Server, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return server.Shutdown(ctx)
}

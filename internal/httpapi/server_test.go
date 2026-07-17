package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/thomdehoog/origoa-foundation/internal/repository"
)

func testServer(t *testing.T) http.Handler {
	t.Helper()
	repo, err := repository.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return New(repo)
}

func call(t *testing.T, handler http.Handler, method, path, body, contentType, tag string) (*http.Response, map[string]any) {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	if tag != "" {
		request.Header.Set("If-Match", tag)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	response := recorder.Result()
	defer response.Body.Close()
	var value map[string]any
	raw, _ := io.ReadAll(response.Body)
	if len(raw) > 0 && strings.HasPrefix(response.Header.Get("Content-Type"), "application/json") && json.Unmarshal(raw, &value) != nil {
		t.Fatalf("invalid JSON response %q", raw)
	}
	return response, value
}

func errorCode(value map[string]any) string {
	errorObject, _ := value["error"].(map[string]any)
	code, _ := errorObject["code"].(string)
	return code
}

func TestHTTPArtifactLifecycleAndSecurityHeaders(t *testing.T) {
	server := testServer(t)

	response, created := call(t, server, "POST", "/api/artifacts", `{"kind":"entry","type":"note","title":"<img src=x onerror=alert(1)>"}`, "application/json", "")
	if response.StatusCode != 201 || response.Header.Get("ETag") == "" || response.Header.Get("Location") == "" {
		t.Fatalf("create failed: %d %#v", response.StatusCode, created)
	}
	if response.Header.Get("Content-Security-Policy") == "" || response.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("security headers missing")
	}
	artifact := created["artifact"].(map[string]any)
	guid := artifact["guid"].(string)
	tag := response.Header.Get("ETag")

	response, _ = call(t, server, "PUT", "/api/artifacts/"+guid, `{"title":"updated"}`, "application/json", "")
	if response.StatusCode != 428 {
		t.Fatalf("missing If-Match got %d", response.StatusCode)
	}
	response, updated := call(t, server, "PUT", "/api/artifacts/"+guid, `{"title":"updated"}`, "application/json", tag)
	if response.StatusCode != 200 || updated["artifact"].(map[string]any)["title"] != "updated" {
		t.Fatalf("update failed: %d %#v", response.StatusCode, updated)
	}
	updatedTag := response.Header.Get("ETag")
	response, value := call(t, server, "PUT", "/api/artifacts/"+guid, `{"title":"stale"}`, "application/json", tag)
	if response.StatusCode != 412 || errorCode(value) != "version_conflict" {
		t.Fatalf("stale update accepted: %d %#v", response.StatusCode, value)
	}

	response, history := call(t, server, "GET", "/api/artifacts/"+guid+"/history", "", "", "")
	if response.StatusCode != 200 || len(history["items"].([]any)) != 2 {
		t.Fatalf("bad history: %d %#v", response.StatusCode, history)
	}
	response, page := call(t, server, "GET", "/", "", "", "")
	if response.StatusCode != 200 || page != nil || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("static app failed: %d", response.StatusCode)
	}
	for _, path := range []string{
		"/api/health", "/api/artifacts", "/api/search?q=updated", "/api/repository/tree",
		"/api/artifacts/" + guid, "/api/artifacts/" + guid + "/schema", "/api/artifacts/" + guid + "/overlay",
		"/api/artifacts/" + guid + "/links", "/api/artifacts/" + guid + "/workflows",
	} {
		response, _ := call(t, server, "GET", path, "", "", "")
		if response.StatusCode != 200 {
			t.Fatalf("GET %s returned %d", path, response.StatusCode)
		}
	}
	response, value = call(t, server, "GET", "/api/artifacts/00000000-0000-4000-8000-000000000000/links", "", "", "")
	if response.StatusCode != 404 || errorCode(value) != "not_found" {
		t.Fatalf("missing artifact links returned %d %#v", response.StatusCode, value)
	}
	response, _ = call(t, server, "DELETE", "/api/artifacts/"+guid, "", "", updatedTag)
	if response.StatusCode != 204 {
		t.Fatalf("delete returned %d", response.StatusCode)
	}
}

func TestHTTPRejectsMalformedAndHostileRequests(t *testing.T) {
	server := testServer(t)
	tests := []struct {
		name, body, contentType string
		status                  int
	}{
		{"wrong content type", `{}`, "text/plain", 415},
		{"malformed", `{`, "application/json", 400},
		{"multiple values", `{} {}`, "application/json", 400},
		{"unknown property", `{"kind":"entry","type":"note","title":"x","admin":true}`, "application/json", 400},
		{"workflow state injection", `{"kind":"entry","type":"note","title":"x","workflows":{"review":"approved"}}`, "application/json", 400},
		{"path traversal", `{"kind":"entry","type":"note","title":"x","path":"../../etc"}`, "application/json", 400},
		{"guid spoof", `{"guid":"00000000-0000-0000-0000-000000000000","kind":"entry","type":"note","title":"x"}`, "application/json", 400},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response, _ := call(t, server, "POST", "/api/artifacts", test.body, test.contentType, "")
			if response.StatusCode != test.status {
				t.Fatalf("got %d, want %d", response.StatusCode, test.status)
			}
		})
	}

	oversized := append([]byte{'"'}, bytes.Repeat([]byte("x"), maxBody+1)...)
	oversized = append(oversized, '"')
	request := httptest.NewRequest("POST", "/api/artifacts", bytes.NewReader(oversized))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	response := recorder.Result()
	defer response.Body.Close()
	if response.StatusCode != 413 {
		t.Fatalf("oversized body got %d", response.StatusCode)
	}

	response, _ = call(t, server, "TRACE", "/api/artifacts", "", "", "")
	if response.StatusCode != 405 {
		t.Fatalf("TRACE got %d", response.StatusCode)
	}
	guid := "00000000-0000-4000-8000-000000000000"
	for _, tag := range []string{"bare", `"""bad"`, `W/"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"`} {
		response, value := call(t, server, "PUT", "/api/artifacts/"+guid, `{}`, "application/json", tag)
		if response.StatusCode != 400 || errorCode(value) != "invalid_etag" {
			t.Fatalf("malformed ETag %q returned %d %#v", tag, response.StatusCode, value)
		}
	}
}

func TestRequestDecoderPreservesLargeIntegers(t *testing.T) {
	request := httptest.NewRequest("POST", "/api/artifacts", strings.NewReader(`{"value":9007199254740993}`))
	request.Header.Set("Content-Type", "application/json")
	var destination map[string]any
	if err := decodeJSON(httptest.NewRecorder(), request, &destination, true); err != nil {
		t.Fatal(err)
	}
	value, ok := destination["value"].(json.Number)
	if !ok || value.String() != "9007199254740993" {
		t.Fatalf("large integer was changed: %T(%v)", destination["value"], destination["value"])
	}
}

func TestConcurrentUpdatesAllowOneWinner(t *testing.T) {
	server := testServer(t)
	response, created := call(t, server, "POST", "/api/artifacts", `{"kind":"entry","type":"note","title":"original"}`, "application/json", "")
	guid := created["artifact"].(map[string]any)["guid"].(string)
	tag := response.Header.Get("ETag")

	statuses := make(chan int, 2)
	var wait sync.WaitGroup
	for _, title := range []string{"first", "second"} {
		wait.Add(1)
		go func() {
			defer wait.Done()
			body := `{"title":"` + title + `"}`
			request := httptest.NewRequest("PUT", "/api/artifacts/"+guid, strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("If-Match", tag)
			recorder := httptest.NewRecorder()
			server.ServeHTTP(recorder, request)
			response := recorder.Result()
			io.Copy(io.Discard, response.Body)
			response.Body.Close()
			statuses <- response.StatusCode
		}()
	}
	wait.Wait()
	close(statuses)
	counts := map[int]int{}
	for status := range statuses {
		counts[status]++
	}
	if counts[200] != 1 || counts[412] != 1 {
		t.Fatalf("concurrent outcomes: %#v", counts)
	}
}

func TestMiddlewareRejectsExcessWork(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	server := &Server{slots: make(chan struct{}, 1)}
	handler := server.middleware(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		response.WriteHeader(http.StatusNoContent)
	}))

	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/slow", nil))
	}()
	<-entered

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest("GET", "/slow", nil))
	if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Retry-After") != "1" {
		t.Fatalf("overload response was %d with Retry-After %q", recorder.Code, recorder.Header().Get("Retry-After"))
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil || errorCode(body) != "server_busy" {
		t.Fatalf("invalid overload response: %q", recorder.Body.String())
	}

	close(release)
	<-done
}

func FuzzExpectedETag(f *testing.F) {
	for _, seed := range []string{"", "bare", `W/"weak"`, `"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"`, `"one", "two"`} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, header string) {
		request := httptest.NewRequest("PUT", "/api/artifacts/id", nil)
		request.Header.Set("If-Match", header)
		value, err := expectedETag(request)
		if err == nil && (value == "" || strings.TrimSpace(header) != `"`+value+`"`) {
			t.Fatalf("invalid ETag accepted: %q -> %q", header, value)
		}
	})
}

func FuzzRequestDecoder(f *testing.F) {
	f.Add([]byte(`{}`), "application/json")
	f.Add([]byte(`{} {}`), "application/json")
	f.Add([]byte(`{}`), "text/plain")
	f.Fuzz(func(t *testing.T, body []byte, contentType string) {
		request := httptest.NewRequest("POST", "/api/artifacts", bytes.NewReader(body))
		request.Header.Set("Content-Type", contentType)
		var destination map[string]any
		_ = decodeJSON(httptest.NewRecorder(), request, &destination, true)
	})
}

package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/thomdehoog/origoa/internal/core"
)

type client struct {
	t   *testing.T
	srv *httptest.Server
}

func newClient(t *testing.T) *client {
	t.Helper()
	f, err := core.Open(t.TempDir() + "/repo.git")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(f))
	t.Cleanup(srv.Close)
	return &client{t: t, srv: srv}
}

// do performs a request and decodes the JSON response.
func (c *client) do(method, path string, body any, headers map[string]string) (int, map[string]any) {
	c.t.Helper()
	var rd *bytes.Reader
	if s, ok := body.(string); ok {
		rd = bytes.NewReader([]byte(s))
	} else if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, c.srv.URL+path, rd)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func (c *client) must(method, path string, body any, wantStatus int) map[string]any {
	c.t.Helper()
	status, out := c.do(method, path, body, nil)
	if status != wantStatus {
		c.t.Fatalf("%s %s = %d (%v), want %d", method, path, status, out, wantStatus)
	}
	return out
}

func guidOf(res map[string]any) string {
	return res["meta"].(map[string]any)["guid"].(string)
}

func TestEndToEnd(t *testing.T) {
	c := newClient(t)

	// Configure a domain purely through the API.
	c.must("PUT", "/api/workflows/dev", map[string]any{
		"id": "dev", "initial": "open", "states": []string{"open", "done"},
		"transitions": []map[string]string{{"from": "open", "to": "done"}},
	}, 200)
	c.must("PUT", "/api/schemas/req", map[string]any{
		"artifactType": "requirement", "kind": "entry", "hidPrefix": "REQ",
		"fields":        []map[string]any{{"id": "priority", "type": "text"}},
		"workflows":     []string{"dev"},
		"relationships": []map[string]any{{"linkType": "verifies", "targetTypes": []string{"testcase"}}},
	}, 200)
	c.must("PUT", "/api/schemas/tc", map[string]any{"artifactType": "testcase"}, 200)

	// Entry lifecycle.
	req1 := c.must("POST", "/api/entries", map[string]any{
		"path": "specs/core", "type": "requirement", "title": "Boot in 2s",
		"fields": map[string]any{"priority": "high"},
	}, 201)
	g := guidOf(req1)
	meta := req1["meta"].(map[string]any)
	if meta["hid"] != "REQ-1" || meta["workflows"].(map[string]any)["dev"] != "open" {
		t.Fatalf("created meta = %v", meta)
	}

	got := c.must("GET", "/api/entries/"+g, nil, 200)
	if got["data"].(map[string]any)["title"] != "Boot in 2s" {
		t.Fatalf("get = %v", got)
	}

	// Documents, links (schema-constrained), comments.
	tc := c.must("POST", "/api/entries", map[string]any{"path": "tests", "type": "testcase", "title": "TC boot"}, 201)
	doc := c.must("POST", "/api/documents", map[string]any{
		"path": "docs", "type": "spec", "title": "System Spec",
		"content": []map[string]any{{"type": "section", "title": "Intro", "children": []map[string]any{{"type": "entryRef", "guid": g}}}},
	}, 201)
	c.must("POST", "/api/links", map[string]any{"type": "verifies", "source": g, "target": guidOf(tc)}, 201)
	c.must("POST", "/api/comments", map[string]any{"subject": g, "text": "please review", "author": "thom"}, 201)

	links := c.must("GET", "/api/artifacts/"+g+"/links", nil, 200)
	if len(links["outgoing"].([]any)) != 1 {
		t.Fatalf("links = %v", links)
	}
	comments := c.must("GET", "/api/artifacts/"+g+"/comments", nil, 200)
	if len(comments["comments"].([]any)) != 1 {
		t.Fatalf("comments = %v", comments)
	}

	// Workflow, search, tree, effective schema, history.
	c.must("POST", "/api/artifacts/"+g+"/transition", map[string]any{"workflow": "dev", "to": "done"}, 200)
	found := c.must("GET", "/api/search?q=boot&kind=entry", nil, 200)
	if len(found["artifacts"].([]any)) != 2 {
		t.Fatalf("search = %v", found)
	}
	tree := c.must("GET", "/api/tree", nil, 200)
	if len(tree["folders"].([]any)) == 0 || len(tree["artifacts"].([]any)) < 3 {
		t.Fatalf("tree = %v", tree)
	}
	schema := c.must("GET", "/api/schemas/effective?type=requirement&path=specs/core", nil, 200)
	if schema["schema"].(map[string]any)["hidPrefix"] != "REQ" {
		t.Fatalf("effective schema = %v", schema)
	}
	hist := c.must("GET", "/api/artifacts/"+g+"/history", nil, 200)
	if len(hist["history"].([]any)) < 2 {
		t.Fatalf("history = %v", hist)
	}

	// Move keeps identity; delete works; reindex agrees.
	c.must("POST", "/api/artifacts/"+g+"/move", map[string]any{"path": "archive"}, 200)
	after := c.must("GET", "/api/entries/"+g, nil, 200)
	if after["meta"].(map[string]any)["folder"] != "archive" {
		t.Fatalf("after move = %v", after)
	}
	if status, _ := c.do("DELETE", "/api/documents/"+guidOf(doc), nil, nil); status != 204 {
		t.Fatalf("delete = %d", status)
	}
	c.must("POST", "/api/admin/reindex", nil, 200)
	c.must("GET", "/api/documents/"+guidOf(doc), nil, 404)
}

func TestOverlayResolveViaAPI(t *testing.T) {
	c := newClient(t)
	base := c.must("POST", "/api/entries", map[string]any{"type": "part", "title": "base",
		"fields": map[string]any{"color": "red", "mass": 5}}, 201)
	overlay := c.must("POST", "/api/entries", map[string]any{"type": "part", "title": "variant",
		"base": guidOf(base), "fields": map[string]any{"color": "blue"}}, 201)

	res := c.must("GET", "/api/entries/"+guidOf(overlay)+"?resolve=1", nil, 200)
	fields := res["resolved"].(map[string]any)["fields"].(map[string]any)
	if fields["color"] != "blue" || fields["mass"] != float64(5) {
		t.Fatalf("resolved = %v", fields)
	}
}

func TestAdversarial(t *testing.T) {
	c := newClient(t)
	seed := c.must("POST", "/api/entries", map[string]any{"type": "part", "title": "seed", "hid": "P-1"}, 201)
	g := guidOf(seed)

	t.Run("path traversal and reserved paths rejected", func(t *testing.T) {
		for _, p := range []string{"../../etc", "a/../../b", ".origoa", "x/.origoa/y",
			"a\\b", "specs/" + g + "/inner"} {
			status, _ := c.do("POST", "/api/entries", map[string]any{"type": "part", "path": p}, nil)
			if status != 400 {
				t.Fatalf("path %q accepted with %d", p, status)
			}
		}
	})

	t.Run("malformed and oversized bodies rejected", func(t *testing.T) {
		for _, b := range []string{`{"unterminated`, `[]`, `null`, `"str"`} {
			if status, _ := c.do("POST", "/api/entries", b, nil); status != 400 {
				t.Fatalf("body %q accepted with %d", b, status)
			}
		}
		huge := `{"type":"part","title":"` + strings.Repeat("x", 5<<20) + `"}`
		if status, _ := c.do("POST", "/api/entries", huge, nil); status != 400 {
			t.Fatal("oversized body accepted")
		}
	})

	t.Run("invalid types and injection strings rejected", func(t *testing.T) {
		for _, typ := range []string{"", "a b", "a/../b", "$(rm -rf)", "--flag"} {
			if status, _ := c.do("POST", "/api/entries", map[string]any{"type": typ}, nil); status != 400 {
				t.Fatalf("type %q accepted", typ)
			}
		}
	})

	t.Run("duplicate HID conflicts", func(t *testing.T) {
		if status, _ := c.do("POST", "/api/entries", map[string]any{"type": "part", "hid": "P-1"}, nil); status != 409 {
			t.Fatal("duplicate HID accepted")
		}
	})

	t.Run("immutable properties rejected on update", func(t *testing.T) {
		for _, patch := range []map[string]any{{"guid": "x"}, {"kind": "document"}, {"type": "other"}, {"workflows": map[string]string{"dev": "done"}}} {
			if status, _ := c.do("PUT", "/api/entries/"+g, patch, nil); status != 400 {
				t.Fatalf("patch %v accepted", patch)
			}
		}
	})

	t.Run("stale If-Match yields conflict", func(t *testing.T) {
		res, _ := http.Get(c.srv.URL + "/api/entries/" + g)
		etag := res.Header.Get("ETag")
		res.Body.Close()
		if status, _ := c.do("PUT", "/api/entries/"+g, map[string]any{"title": "v2"}, map[string]string{"If-Match": etag}); status != 200 {
			t.Fatal("fresh If-Match rejected")
		}
		if status, _ := c.do("PUT", "/api/entries/"+g, map[string]any{"title": "v3"}, map[string]string{"If-Match": etag}); status != 409 {
			t.Fatal("stale If-Match accepted")
		}
	})

	t.Run("links to and from ghosts rejected", func(t *testing.T) {
		ghost := core.NewGUID()
		for _, l := range []map[string]any{
			{"type": "rel", "source": ghost, "target": g},
			{"type": "rel", "source": g, "target": ghost},
			{"type": "rel", "source": g, "target": ""},
		} {
			if status, _ := c.do("POST", "/api/links", l, nil); status != 400 {
				t.Fatalf("link %v accepted", l)
			}
		}
	})

	t.Run("schema-constrained link types enforced", func(t *testing.T) {
		c.must("PUT", "/api/schemas/strict", map[string]any{
			"artifactType": "strict",
			"relationships": []map[string]any{{"linkType": "only", "targetTypes": []string{"strict"}}},
		}, 200)
		a := guidOf(c.must("POST", "/api/entries", map[string]any{"type": "strict"}, 201))
		if status, _ := c.do("POST", "/api/links", map[string]any{"type": "other", "source": a, "target": g}, nil); status != 400 {
			t.Fatal("undeclared link type accepted")
		}
		if status, _ := c.do("POST", "/api/links", map[string]any{"type": "only", "source": a, "target": g}, nil); status != 400 {
			t.Fatal("disallowed target type accepted")
		}
	})

	t.Run("comment on ghost or foreign parent rejected", func(t *testing.T) {
		if status, _ := c.do("POST", "/api/comments", map[string]any{"subject": core.NewGUID(), "text": "x"}, nil); status != 400 {
			t.Fatal("comment on ghost accepted")
		}
		other := guidOf(c.must("POST", "/api/entries", map[string]any{"type": "part"}, 201))
		cm := guidOf(c.must("POST", "/api/comments", map[string]any{"subject": other, "text": "x"}, 201))
		if status, _ := c.do("POST", "/api/comments", map[string]any{"subject": g, "text": "y", "parent": cm}, nil); status != 400 {
			t.Fatal("cross-subject reply accepted")
		}
	})

	t.Run("unknown artifacts are 404", func(t *testing.T) {
		ghost := core.NewGUID()
		for _, p := range []string{"/api/entries/" + ghost, "/api/artifacts/" + ghost + "/links",
			"/api/artifacts/" + ghost + "/history", "/api/workflows/nosuch"} {
			if status, _ := c.do("GET", p, nil, nil); status != 404 {
				t.Fatalf("GET %s != 404", p)
			}
		}
	})

	t.Run("invalid workflow definitions rejected", func(t *testing.T) {
		bad := []map[string]any{
			{"id": "w", "initial": "ghost", "states": []string{"open"}},
			{"id": "w", "initial": "open", "states": []string{"open"},
				"transitions": []map[string]string{{"from": "open", "to": "ghost"}}},
			{"initial": "open", "states": []string{"open"}},
		}
		for _, wf := range bad {
			if status, _ := c.do("PUT", "/api/workflows/w", wf, nil); status != 400 {
				t.Fatalf("workflow %v accepted", wf)
			}
		}
	})

	t.Run("concurrent creates all persist", func(t *testing.T) {
		const n = 6
		done := make(chan int, n)
		for i := 0; i < n; i++ {
			go func(i int) {
				status, _ := c.do("POST", "/api/entries", map[string]any{"type": "part", "title": fmt.Sprintf("c%d", i)}, nil)
				done <- status
			}(i)
		}
		for i := 0; i < n; i++ {
			if s := <-done; s != 201 {
				t.Fatalf("concurrent create = %d", s)
			}
		}
	})
}

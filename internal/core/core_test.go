package core

import (
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/thomdehoog/origoa/internal/ojson"
)

// pgTestDSN prepares a clean, package-private schema namespace and returns a
// DSN scoped to it, or "" when ORIGOA_TEST_DSN is unset. The namespace keeps
// concurrently tested packages from clobbering each other's tables.
func pgTestDSN(t *testing.T) string {
	base := os.Getenv("ORIGOA_TEST_DSN")
	if base == "" {
		return ""
	}
	db, err := sql.Open("postgres", base)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE SCHEMA IF NOT EXISTS origoa_core`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE IF EXISTS origoa_core.artifacts, origoa_core.config_files, origoa_core.repo_state`); err != nil {
		t.Fatal(err)
	}
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	return base + sep + "search_path=origoa_core"
}

// openTest opens a Foundation with the PostgreSQL projection when
// ORIGOA_TEST_DSN is set (resetting the namespace), else in-memory.
func openTest(t *testing.T, gitDir string) (*Foundation, error) {
	t.Helper()
	if dsn := pgTestDSN(t); dsn != "" {
		return OpenPostgres(gitDir, dsn)
	}
	return Open(gitDir)
}

func testFoundation(t *testing.T) *Foundation {
	t.Helper()
	f, err := openTest(t, t.TempDir()+"/repo.git")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func body(t *testing.T, js string) *ojson.Obj {
	t.Helper()
	o, err := ojson.Parse([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestCleanFolder(t *testing.T) {
	valid := map[string]string{"": "", "/": "", "a/b": "a/b", " /a/b/ ": "a/b"}
	for in, want := range valid {
		got, err := CleanFolder(in)
		if err != nil || got != want {
			t.Fatalf("CleanFolder(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	invalid := []string{"../x", "a/../b", "a//b", ".origoa", "a/.origoa/b",
		"a\\b", "a/" + NewGUID() + "/b", "a/./b"}
	for _, in := range invalid {
		if _, err := CleanFolder(in); err == nil {
			t.Fatalf("CleanFolder(%q) accepted, want error", in)
		}
	}
}

func TestSchemaComposition(t *testing.T) {
	f := testFoundation(t)
	must(t, f.PutSchema("", "req", &Schema{
		ArtifactType: "requirement", Kind: KindEntry, HIDPrefix: "REQ",
		Fields: []Field{{ID: "priority", Type: "enum", Options: []string{"low", "high"}}, {ID: "owner", Type: "text"}},
	}))
	must(t, f.PutSchema("proj", "req", &Schema{
		ArtifactType: "requirement",
		Fields:       []Field{{ID: "priority", Type: "enum", Options: []string{"p1", "p2", "p3"}}, {ID: "cost", Type: "number"}},
	}))

	s, err := f.EffectiveSchema("requirement", "proj/sub")
	if err != nil {
		t.Fatal(err)
	}
	// Deeper definition replaces field in place, new fields append, inherited stay.
	if len(s.Fields) != 3 || s.Fields[0].ID != "priority" || len(s.Fields[0].Options) != 3 ||
		s.Fields[1].ID != "owner" || s.Fields[2].ID != "cost" {
		t.Fatalf("composed fields = %+v", s.Fields)
	}
	if s.HIDPrefix != "REQ" {
		t.Fatalf("HIDPrefix not inherited: %+v", s)
	}

	// inheritance: off severs everything above.
	must(t, f.PutSchema("island", "req", &Schema{
		ArtifactType: "requirement", Inheritance: "off",
		Fields: []Field{{ID: "only", Type: "text"}},
	}))
	s, err = f.EffectiveSchema("requirement", "island")
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Fields) != 1 || s.Fields[0].ID != "only" || s.HIDPrefix != "" {
		t.Fatalf("inheritance off leaked parent definitions: %+v", s)
	}
	// Root scope is unaffected.
	if _, err := f.EffectiveSchema("requirement", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := f.EffectiveSchema("nosuch", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown type: %v", err)
	}
}

func TestHIDGenerationAndUniqueness(t *testing.T) {
	f := testFoundation(t)
	must(t, f.PutSchema("", "req", &Schema{ArtifactType: "requirement", HIDPrefix: "REQ"}))

	m1, err := f.CreateArtifact(KindEntry, "", "requirement", body(t, `{"title":"one"}`))
	must(t, err)
	m2, err := f.CreateArtifact(KindEntry, "", "requirement", body(t, `{"title":"two"}`))
	must(t, err)
	if m1.HID != "REQ-1" || m2.HID != "REQ-2" {
		t.Fatalf("HIDs = %q, %q", m1.HID, m2.HID)
	}
	// Duplicate explicit HID rejected.
	if _, err := f.CreateArtifact(KindEntry, "", "requirement", body(t, `{"hid":"REQ-1"}`)); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate HID: %v", err)
	}
	// Changing an HID frees the old one; history stays in Git.
	_, err = f.UpdateArtifact(m1.GUID, body(t, `{"hid":"REQ-100"}`), "")
	must(t, err)
	m3, err := f.CreateArtifact(KindEntry, "", "requirement", body(t, `{"hid":"REQ-1"}`))
	must(t, err)
	if m3.HID != "REQ-1" {
		t.Fatalf("freed HID not reusable: %q", m3.HID)
	}
	// Auto-generation continues past the highest number.
	m4, err := f.CreateArtifact(KindEntry, "", "requirement", body(t, `{}`))
	must(t, err)
	if m4.HID != "REQ-101" {
		t.Fatalf("next HID = %q, want REQ-101", m4.HID)
	}
}

func TestOverlayResolutionAndCycles(t *testing.T) {
	f := testFoundation(t)
	a, err := f.CreateArtifact(KindEntry, "", "part", body(t, `{"title":"base","fields":{"color":"red","mass":"5"}}`))
	must(t, err)
	b, err := f.CreateArtifact(KindEntry, "", "part", body(t, `{"title":"variant","base":"`+a.GUID+`","fields":{"color":"blue"}}`))
	must(t, err)

	fields, chain, err := f.ResolveOverlay(b.GUID)
	must(t, err)
	if string(fields["color"]) != `"blue"` || string(fields["mass"]) != `"5"` {
		t.Fatalf("resolved fields = %v", fields)
	}
	if len(chain) != 2 || chain[0] != b.GUID || chain[1] != a.GUID {
		t.Fatalf("chain = %v", chain)
	}
	// Making the base point at its own overlay must be rejected (cycle).
	if _, err := f.UpdateArtifact(a.GUID, body(t, `{"base":"`+b.GUID+`"}`), ""); !errors.Is(err, ErrValidation) {
		t.Fatalf("cycle accepted: %v", err)
	}
	// Self-reference rejected.
	if _, err := f.UpdateArtifact(a.GUID, body(t, `{"base":"`+a.GUID+`"}`), ""); !errors.Is(err, ErrValidation) {
		t.Fatalf("self base accepted: %v", err)
	}
	// Unknown base rejected.
	if _, err := f.CreateArtifact(KindEntry, "", "part", body(t, `{"base":"`+NewGUID()+`"}`)); !errors.Is(err, ErrValidation) {
		t.Fatalf("unknown base accepted: %v", err)
	}
}

func TestWorkflowTransitions(t *testing.T) {
	f := testFoundation(t)
	must(t, f.PutWorkflow("", "dev", &Workflow{
		ID: "dev", Initial: "open", States: []string{"open", "review", "done"},
		Transitions: []Transition{{From: "open", To: "review"}, {From: "review", To: "done"}, {From: "review", To: "open"}},
	}))
	must(t, f.PutSchema("", "ticket", &Schema{ArtifactType: "ticket", Workflows: []string{"dev"}}))

	m, err := f.CreateArtifact(KindEntry, "", "ticket", body(t, `{"title":"t"}`))
	must(t, err)
	if m.Workflows["dev"] != "open" {
		t.Fatalf("initial state = %v", m.Workflows)
	}
	// Skipping a state is rejected.
	if _, err := f.Transition(m.GUID, "dev", "done"); !errors.Is(err, ErrValidation) {
		t.Fatalf("invalid transition accepted: %v", err)
	}
	m, err = f.Transition(m.GUID, "dev", "review")
	must(t, err)
	if m.Workflows["dev"] != "review" {
		t.Fatalf("state = %v", m.Workflows)
	}
	// Unassigned workflow rejected.
	if _, err := f.Transition(m.GUID, "publish", "review"); !errors.Is(err, ErrValidation) {
		t.Fatalf("unassigned workflow accepted: %v", err)
	}
	// The transition is recorded as a structured commit.
	log, err := f.History(m.GUID, 10)
	must(t, err)
	if !strings.Contains(log[0].Subject, "transitioned from open to review") {
		t.Fatalf("commit subject = %q", log[0].Subject)
	}
}

func TestMoveKeepsIdentityAndLinks(t *testing.T) {
	f := testFoundation(t)
	a, err := f.CreateArtifact(KindEntry, "src", "part", body(t, `{"title":"a"}`))
	must(t, err)
	b, err := f.CreateArtifact(KindEntry, "src", "part", body(t, `{"title":"b"}`))
	must(t, err)
	l, err := f.CreateLink("relates", a.GUID, b.GUID, nil)
	must(t, err)

	moved, err := f.MoveArtifact(a.GUID, "dst/deep")
	must(t, err)
	if moved.Folder != "dst/deep" || moved.GUID != a.GUID {
		t.Fatalf("moved meta = %+v", moved)
	}
	in, out := f.Links(a.GUID)
	if len(out) != 1 || out[0].GUID != l.GUID || len(in) != 0 {
		t.Fatalf("links after move: in=%v out=%v", in, out)
	}
	// Old location is gone.
	for _, m := range f.List("", "", "src", false) {
		if m.GUID == a.GUID {
			t.Fatal("artifact still listed in old folder")
		}
	}
}

func TestRebuildFromGitAlone(t *testing.T) {
	dir := t.TempDir() + "/repo.git"
	f, err := openTest(t, dir)
	must(t, err)
	must(t, f.PutSchema("", "req", &Schema{ArtifactType: "requirement", HIDPrefix: "REQ"}))
	m, err := f.CreateArtifact(KindEntry, "a/b", "requirement", body(t, `{"title":"persisted"}`))
	must(t, err)
	c, err := f.CreateComment(m.GUID, "note", "", "tester")
	must(t, err)

	// A fresh Foundation over the same Git dir must project identical state
	// (with Postgres this exercises the full Sync rebuild after a reset).
	f2, err := openTest(t, dir)
	must(t, err)
	defer f2.Close()
	m2, err := f2.Meta(m.GUID)
	must(t, err)
	if m2.Title != "persisted" || m2.HID != m.HID || m2.Folder != "a/b" {
		t.Fatalf("rebuilt meta = %+v", m2)
	}
	if cs := f2.Comments(m.GUID); len(cs) != 1 || cs[0].GUID != c.GUID {
		t.Fatalf("rebuilt comments = %v", cs)
	}
	if len(f2.Search("persisted", "", "")) != 1 {
		t.Fatal("search index not rebuilt")
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

package core

import (
	"testing"

	"github.com/thomdehoog/origoa/internal/gitx"
)

// Postgres-specific behavior: processed_hash fast start and divergence
// recovery. Requires ORIGOA_TEST_DSN; skipped otherwise.
func TestPostgresFastStartAndRecovery(t *testing.T) {
	dsn := pgTestDSN(t)
	if dsn == "" {
		t.Skip("ORIGOA_TEST_DSN not set")
	}
	dir := t.TempDir() + "/repo.git"
	f, err := OpenPostgres(dir, dsn)
	must(t, err)
	m, err := f.CreateArtifact(KindEntry, "a", "part", body(t, `{"title":"persisted"}`))
	must(t, err)
	head := f.Head()
	must(t, f.Close())

	// Reopen WITHOUT resetting tables: processed_hash matches HEAD, so the
	// stored projection is reused as-is.
	f2, err := OpenPostgres(dir, dsn)
	must(t, err)
	if f2.Head() != head {
		t.Fatalf("fast start head = %s, want %s", f2.Head(), head)
	}
	got, err := f2.Meta(m.GUID)
	must(t, err)
	if got.Title != "persisted" {
		t.Fatalf("meta after fast start = %+v", got)
	}
	must(t, f2.Close())

	// A foreign commit directly to the bare repo diverges Git from the
	// projection; reopening must detect the mismatch and rebuild everything,
	// including the full-text index.
	g := &gitx.Repo{Dir: dir}
	foreign := NewGUID()
	_, err = g.Commit("manual edit", []gitx.Op{{
		Path:    "manual/" + foreign + "/" + ArtifactFile,
		Content: []byte(`{"guid":"` + foreign + `","kind":"entry","type":"part","title":"foreign"}`),
	}})
	must(t, err)

	f3, err := OpenPostgres(dir, dsn)
	must(t, err)
	defer f3.Close()
	fm, err := f3.Meta(foreign)
	must(t, err)
	if fm.Title != "foreign" || fm.Folder != "manual" {
		t.Fatalf("foreign artifact not projected: %+v", fm)
	}
	if len(f3.Search("foreign", "", "")) != 1 {
		t.Fatal("full-text index not rebuilt")
	}
}

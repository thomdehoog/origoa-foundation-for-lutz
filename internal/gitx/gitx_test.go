package gitx

import (
	"fmt"
	"sync"
	"testing"
)

func testRepo(t *testing.T) *Repo {
	t.Helper()
	r, err := Init(t.TempDir() + "/repo.git")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestCommitReadDelete(t *testing.T) {
	r := testRepo(t)
	if head, _ := r.Head(); head != "" {
		t.Fatalf("fresh repo has head %q", head)
	}
	c1, err := r.Commit("add a", []Op{{Path: "dir/a.json", Content: []byte(`{"a":1}`)}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.ReadBlob(c1, "dir/a.json")
	if err != nil || string(got) != `{"a":1}` {
		t.Fatalf("ReadBlob = %q, %v", got, err)
	}
	if _, err := r.Commit("del a", []Op{{Path: "dir/a.json", Delete: true}}); err != nil {
		t.Fatal(err)
	}
	head, _ := r.Head()
	entries, err := r.ListTree(head, "")
	if err != nil || len(entries) != 0 {
		t.Fatalf("tree after delete = %v, %v", entries, err)
	}
	log, err := r.Log("", 10)
	if err != nil || len(log) != 2 || log[0].Subject != "del a" {
		t.Fatalf("log = %v, %v", log, err)
	}
}

func TestConcurrentCommitsAllLand(t *testing.T) {
	r := testRepo(t)
	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			path := fmt.Sprintf("f%d.json", i)
			_, errs[i] = r.Commit(fmt.Sprintf("add %d", i), []Op{{Path: path, Content: []byte("{}")}})
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
	}
	head, _ := r.Head()
	entries, _ := r.ListTree(head, "")
	if len(entries) != n {
		t.Fatalf("got %d files, want %d (CAS lost updates)", len(entries), n)
	}
}

func TestReadBlobsBatch(t *testing.T) {
	r := testRepo(t)
	if _, err := r.Commit("seed", []Op{
		{Path: "a", Content: []byte("alpha")},
		{Path: "b", Content: []byte("beta\nwith\nnewlines")},
	}); err != nil {
		t.Fatal(err)
	}
	head, _ := r.Head()
	entries, _ := r.ListTree(head, "")
	shas := []string{entries[0].SHA, entries[1].SHA}
	blobs, err := r.ReadBlobs(shas)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		want := map[string]string{"a": "alpha", "b": "beta\nwith\nnewlines"}[e.Path]
		if string(blobs[e.SHA]) != want {
			t.Fatalf("blob %s = %q, want %q", e.Path, blobs[e.SHA], want)
		}
	}
}

func TestBlobSHAMatchesGit(t *testing.T) {
	r := testRepo(t)
	content := []byte(`{"x": 1}`)
	if _, err := r.Commit("x", []Op{{Path: "x.json", Content: content}}); err != nil {
		t.Fatal(err)
	}
	head, _ := r.Head()
	entries, _ := r.ListTree(head, "")
	if entries[0].SHA != BlobSHA(content) {
		t.Fatalf("BlobSHA = %s, git says %s", BlobSHA(content), entries[0].SHA)
	}
}

package repository

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testRepository(t *testing.T) (*Repository, context.Context) {
	t.Helper()
	ctx := context.Background()
	repo, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return repo, ctx
}

func assertRepoError(t *testing.T, err error, status int, code string) {
	t.Helper()
	var target *Error
	if !errors.As(err, &target) || target.Status != status || target.Code != code {
		t.Fatalf("got %v, want repository error %d/%s", err, status, code)
	}
}

func configure(t *testing.T, repo *Repository, ctx context.Context, relativePath string, value any) {
	t.Helper()
	file := filepath.Join(repo.root, filepath.FromSlash(relativePath))
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.git(ctx, "add", "--", relativePath); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.git(ctx, "commit", "--quiet", "-m", "Configure test repository", "--", relativePath); err != nil {
		t.Fatal(err)
	}
}

func TestCRUDHistoryAndReferences(t *testing.T) {
	repo, ctx := testRepository(t)
	first, err := repo.Create(ctx, CreateInput{Kind: Entry, Type: "note", Title: "Alpha", HID: "NOTE-1", Path: "teams/core"})
	if err != nil {
		t.Fatal(err)
	}
	if !uuidPattern.MatchString(first.Artifact.GUID) || first.Path != "teams/core" || first.ETag == "" {
		t.Fatalf("bad created artifact: %#v", first)
	}

	_, err = repo.Create(ctx, CreateInput{Kind: Entry, Type: "note", Title: "Duplicate HID", HID: "note-1"})
	assertRepoError(t, err, 409, "duplicate_hid")

	updated, err := repo.Update(ctx, first.Artifact.GUID, map[string]any{"title": "Alpha updated", "fields": map[string]any{"rank": 1}}, first.ETag)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Artifact.Title != "Alpha updated" || updated.ETag == first.ETag {
		t.Fatalf("update did not change artifact: %#v", updated)
	}
	_, err = repo.Update(ctx, first.Artifact.GUID, map[string]any{"title": "stale"}, first.ETag)
	assertRepoError(t, err, 412, "version_conflict")
	_, err = repo.Update(ctx, first.Artifact.GUID, map[string]any{"guid": newTestGUID()}, updated.ETag)
	assertRepoError(t, err, 400, "immutable_or_unknown_property")

	second, err := repo.Create(ctx, CreateInput{Kind: Entry, Type: "note", Title: "Beta"})
	if err != nil {
		t.Fatal(err)
	}
	link, err := repo.Create(ctx, CreateInput{
		Kind: Link, Type: "dependency", Title: "Alpha depends on Beta", LinkType: "depends-on",
		Source: first.Artifact.GUID, Target: second.Artifact.GUID,
	})
	if err != nil {
		t.Fatal(err)
	}
	relations, err := repo.Links(first.Artifact.GUID)
	if err != nil || len(relations.Outgoing) != 1 || len(relations.Incoming) != 0 {
		t.Fatalf("bad links: %#v, %v", relations, err)
	}
	if err := repo.Delete(ctx, second.Artifact.GUID, second.ETag); err == nil {
		t.Fatal("deleted a referenced artifact")
	} else {
		assertRepoError(t, err, 409, "artifact_referenced")
	}
	if err := repo.Delete(ctx, link.Artifact.GUID, link.ETag); err != nil {
		t.Fatal(err)
	}
	if err := repo.Delete(ctx, second.Artifact.GUID, second.ETag); err != nil {
		t.Fatal(err)
	}

	history, err := repo.History(ctx, first.Artifact.GUID, 10)
	if err != nil || len(history) != 2 || !strings.Contains(history[0].Subject, "updated") {
		t.Fatalf("bad history: %#v, %v", history, err)
	}
}

func TestCommittedHeadIsAuthoritative(t *testing.T) {
	repo, ctx := testRepository(t)
	item, err := repo.Create(ctx, CreateInput{Kind: Entry, Type: "note", Title: "Committed"})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := repo.get(item.Artifact.GUID, mustScan(t, repo))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stored.file, []byte(`{"guid":"broken"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := repo.Get(item.Artifact.GUID)
	if err != nil || got.Artifact.Title != "Committed" || got.ETag != item.ETag {
		t.Fatalf("worktree mutation became authoritative: %#v, %v", got, err)
	}
	if err := os.Remove(stored.file); err != nil {
		t.Fatal(err)
	}
	got, err = repo.Get(item.Artifact.GUID)
	if err != nil || got.Artifact.Title != "Committed" {
		t.Fatalf("worktree deletion became authoritative: %#v, %v", got, err)
	}
}

func TestSchemasOverlaysAndWorkflows(t *testing.T) {
	repo, ctx := testRepository(t)
	configure(t, repo, ctx, ".origoa/schemas/requirement.json", map[string]any{
		"id": "requirement",
		"fields": map[string]any{
			"priority": map[string]any{"type": "enumeration", "required": true, "values": []string{"low", "high"}},
			"source":   map[string]any{"type": "hyperlink"},
			"settings": map[string]any{"type": "object"},
		},
		"workflows": []string{"review"},
	})
	configure(t, repo, ctx, "teams/core/.origoa/schemas/requirement.json", map[string]any{
		"fields": map[string]any{"owner": map[string]any{"type": "text", "required": true}},
	})
	configure(t, repo, ctx, ".origoa/workflows/review.json", map[string]any{
		"id": "review", "initial": "draft", "states": []string{"draft", "approved"},
		"transitions": []map[string]string{{"id": "approve", "from": "draft", "to": "approved"}},
	})

	_, err := repo.Create(ctx, CreateInput{Kind: Entry, Type: "requirement", Title: "Missing fields", Path: "teams/core"})
	assertRepoError(t, err, 400, "required_field")
	_, err = repo.Create(ctx, CreateInput{Kind: Entry, Type: "requirement", Title: "Bad URL", Path: "teams/core", Fields: map[string]any{
		"priority": "high", "owner": "Ada", "source": "javascript:alert(1)",
	}})
	assertRepoError(t, err, 400, "invalid_field")

	base, err := repo.Create(ctx, CreateInput{Kind: Entry, Type: "requirement", Title: "Base", Path: "teams/core", Fields: map[string]any{
		"priority": "high", "owner": "Ada", "source": "https://example.com/source", "settings": map[string]any{"a": 1, "b": 2},
	}})
	if err != nil {
		t.Fatal(err)
	}
	overlay, err := repo.Create(ctx, CreateInput{Kind: Entry, Type: "requirement", Title: "Variant", Path: "teams/core", Base: base.Artifact.GUID, Fields: map[string]any{
		"priority": "low", "settings": map[string]any{"a": 3},
	}})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := repo.ResolveOverlay(overlay.Artifact.GUID)
	settings := resolved.Artifact.Fields["settings"].(map[string]any)
	if err != nil || len(resolved.Chain) != 2 || resolved.Artifact.Fields["source"] != "https://example.com/source" || resolved.Artifact.Fields["owner"] != "Ada" || resolved.Artifact.Fields["priority"] != "low" || settings["b"] != nil {
		t.Fatalf("bad overlay: %#v, %v", resolved, err)
	}

	workflows, err := repo.Workflows(base.Artifact.GUID)
	if err != nil || len(workflows) != 1 || workflows[0].State != "draft" || len(workflows[0].Transitions) != 1 {
		t.Fatalf("bad workflows: %#v, %v", workflows, err)
	}
	transitioned, err := repo.Transition(ctx, base.Artifact.GUID, "review", "approve", base.ETag)
	if err != nil || transitioned.Artifact.Workflows["review"] != "approved" {
		t.Fatalf("transition failed: %#v, %v", transitioned, err)
	}
	_, err = repo.Transition(ctx, base.Artifact.GUID, "review", "approve", transitioned.ETag)
	assertRepoError(t, err, 409, "transition_not_allowed")
	_, err = repo.Update(ctx, base.Artifact.GUID, map[string]any{"workflows": map[string]string{"review": "draft"}}, transitioned.ETag)
	assertRepoError(t, err, 400, "immutable_or_unknown_property")
	_, err = repo.Transition(ctx, base.Artifact.GUID, "unassigned", "approve", transitioned.ETag)
	assertRepoError(t, err, 409, "workflow_not_assigned")

	_, err = repo.Update(ctx, base.Artifact.GUID, map[string]any{"base": overlay.Artifact.GUID}, transitioned.ETag)
	assertRepoError(t, err, 409, "overlay_cycle")
}

func TestSearchTreeAndAdversarialInput(t *testing.T) {
	repo, ctx := testRepository(t)
	for _, title := range []string{"Security requirement", "Release note", "Design note"} {
		if _, err := repo.Create(ctx, CreateInput{Kind: Entry, Type: "note", Title: title}); err != nil {
			t.Fatal(err)
		}
	}
	results, err := repo.Search(SearchInput{Query: "note", Limit: 1})
	if err != nil || len(results) != 1 {
		t.Fatalf("bad search: %#v, %v", results, err)
	}
	tree, err := repo.Tree()
	if err != nil || tree["folders"] == nil {
		t.Fatalf("bad tree: %#v, %v", tree, err)
	}

	for _, path := range []string{"../escape", "/absolute", ".git/hooks", "safe/../../escape", "safe\\escape"} {
		_, err := repo.Create(ctx, CreateInput{Kind: Entry, Type: "note", Title: "Attack", Path: path})
		assertRepoError(t, err, 400, "invalid_path")
	}
	_, err = repo.Create(ctx, CreateInput{Kind: Link, Type: "dependency", Title: "Broken", LinkType: "depends-on", Source: newTestGUID(), Target: newTestGUID()})
	assertRepoError(t, err, 400, "broken_reference")
	_, err = repo.Search(SearchInput{Limit: 201})
	assertRepoError(t, err, 400, "invalid_limit")

	deep := map[string]any{}
	cursor := deep
	for range 40 {
		next := map[string]any{}
		cursor["next"] = next
		cursor = next
	}
	_, err = repo.Create(ctx, CreateInput{Kind: Entry, Type: "note", Title: "Too deep", Fields: deep})
	assertRepoError(t, err, 400, "payload_too_complex")
	_, err = repo.Create(ctx, CreateInput{Kind: Entry, Type: "note", Title: "Pollution", Fields: map[string]any{"__proto__": map[string]any{"admin": true}}})
	assertRepoError(t, err, 400, "invalid_key")

	escape := t.TempDir()
	if err := os.Symlink(escape, filepath.Join(repo.root, "redirect")); err != nil {
		t.Fatal(err)
	}
	_, err = repo.Create(ctx, CreateInput{Kind: Entry, Type: "note", Title: "Symlink escape", Path: "redirect"})
	assertRepoError(t, err, 400, "invalid_path")
	entries, err := os.ReadDir(escape)
	if err != nil || len(entries) != 0 {
		t.Fatalf("symlink target was modified: %v, %v", entries, err)
	}
}

func TestCaseInsensitiveAndSchemaReferenceIntegrity(t *testing.T) {
	repo, ctx := testRepository(t)
	configure(t, repo, ctx, ".origoa/schemas/note.json", map[string]any{
		"id": "note", "fields": map[string]any{"related": map[string]any{"type": "artifact-reference"}},
	})
	target, err := repo.Create(ctx, CreateInput{Kind: Entry, Type: "note", Title: "Target"})
	if err != nil {
		t.Fatal(err)
	}
	referrer, err := repo.Create(ctx, CreateInput{Kind: Entry, Type: "note", Title: "Referrer", Fields: map[string]any{"related": strings.ToUpper(target.Artifact.GUID)}})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Delete(ctx, target.Artifact.GUID, target.ETag); err == nil {
		t.Fatal("deleted schema-referenced artifact")
	} else {
		assertRepoError(t, err, 409, "artifact_referenced")
	}
	updated, err := repo.Update(ctx, referrer.Artifact.GUID, map[string]any{"fields": map[string]any{}}, referrer.ETag)
	if err != nil {
		t.Fatal(err)
	}

	other, err := repo.Create(ctx, CreateInput{Kind: Entry, Type: "note", Title: "Other"})
	if err != nil {
		t.Fatal(err)
	}
	link, err := repo.Create(ctx, CreateInput{Kind: Link, Type: "dependency", Title: "Mixed case", LinkType: "depends-on", Source: strings.ToUpper(target.Artifact.GUID), Target: strings.ToUpper(other.Artifact.GUID)})
	if err != nil {
		t.Fatal(err)
	}
	if link.Artifact.Source != target.Artifact.GUID || link.Artifact.Target != other.Artifact.GUID {
		t.Fatalf("references were not canonicalized: %#v", link.Artifact)
	}
	relations, err := repo.Links(strings.ToUpper(target.Artifact.GUID))
	if err != nil || len(relations.Outgoing) != 1 {
		t.Fatalf("mixed-case link lookup failed: %#v, %v", relations, err)
	}
	if err := repo.Delete(ctx, target.Artifact.GUID, target.ETag); err == nil {
		t.Fatal("deleted link-referenced artifact")
	}
	if err := repo.Delete(ctx, link.Artifact.GUID, link.ETag); err != nil {
		t.Fatal(err)
	}
	relations, err = repo.Links(target.Artifact.GUID)
	if err != nil || len(relations.Incoming)+len(relations.Outgoing) != 0 {
		t.Fatalf("deleted link remains visible: %#v, %v", relations, err)
	}
	storedReferrer, err := repo.Get(referrer.Artifact.GUID)
	if err != nil || len(storedReferrer.Artifact.Fields) != 0 {
		t.Fatalf("field reference was not removed: %#v, %v", storedReferrer, err)
	}
	if err := repo.Delete(ctx, target.Artifact.GUID, target.ETag); err != nil {
		t.Fatal(err)
	}
	_ = updated

	_, err = repo.Create(ctx, CreateInput{Kind: Entry, Type: "note", Title: "Broken ref", Fields: map[string]any{"related": newTestGUID()}})
	assertRepoError(t, err, 400, "broken_reference")
}

func TestStrictStoredArtifactDecoding(t *testing.T) {
	repo, ctx := testRepository(t)
	item, err := repo.Create(ctx, CreateInput{Kind: Entry, Type: "note", Title: "Strict"})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.ToSlash(filepath.Join(item.Path, item.Artifact.GUID, "artifact.json"))
	file := filepath.Join(repo.root, filepath.FromSlash(path))
	raw := strings.Replace(string(mustRead(t, file)), `"title": "Strict"`, `"title": "Strict",\n  "unknown": true`, 1)
	if err := os.WriteFile(file, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.git(ctx, "add", "--", path); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.git(ctx, "commit", "--quiet", "-m", "Inject unknown field", "--", path); err != nil {
		t.Fatal(err)
	}
	_, err = repo.Get(item.Artifact.GUID)
	assertRepoError(t, err, 500, "repository_corrupt")
}

func mustScan(t *testing.T, repo *Repository) map[string]StoredArtifact {
	t.Helper()
	all, err := repo.scan()
	if err != nil {
		t.Fatal(err)
	}
	return all
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func newTestGUID() string {
	guid, err := newGUID()
	if err != nil {
		panic(err)
	}
	return guid
}

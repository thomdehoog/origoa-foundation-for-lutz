package repository

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	pathpkg "path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestConcurrentRepositoryHandlesPreserveUniqueHID(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()
	first, err := Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	results := make(chan error, 2)
	for index, repo := range []*Repository{first, second} {
		go func() {
			<-start
			_, err := repo.Create(ctx, CreateInput{Kind: Entry, Type: "note", Title: fmt.Sprintf("Concurrent %d", index), HID: "UNIQUE-1"})
			results <- err
		}()
	}
	close(start)
	var successes, conflicts int
	for range 2 {
		err := <-results
		if err == nil {
			successes++
			continue
		}
		var repoError *Error
		if errors.As(err, &repoError) && repoError.Code == "duplicate_hid" {
			conflicts++
			continue
		}
		t.Fatalf("unexpected concurrent error: %v", err)
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("unique HID race: %d successes, %d conflicts", successes, conflicts)
	}
}

func TestConcurrentProcessesPreserveUniqueHID(t *testing.T) {
	root := t.TempDir()
	if _, err := Open(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	startAt := time.Now().Add(500 * time.Millisecond).Format(time.RFC3339Nano)
	type processResult struct {
		output []byte
		err    error
	}
	results := make(chan processResult, 4)
	for index := range 4 {
		command := exec.Command(executable, "-test.run=^TestRepositoryProcessHelper$")
		command.Env = append(os.Environ(),
			"ORIGOA_TEST_REPOSITORY="+root,
			"ORIGOA_TEST_START_AT="+startAt,
			fmt.Sprintf("ORIGOA_TEST_TITLE=Process %d", index),
		)
		go func() {
			output, err := command.CombinedOutput()
			results <- processResult{output: output, err: err}
		}()
	}
	for range 4 {
		result := <-results
		if result.err != nil {
			t.Fatalf("repository process failed: %v: %s", result.err, result.output)
		}
	}

	repo, err := Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	items, err := repo.List(Filters{})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Artifact.HID != "PROCESS-UNIQUE" {
		t.Fatalf("cross-process uniqueness failed: %#v", items)
	}
}

func TestRepositoryProcessHelper(t *testing.T) {
	root := os.Getenv("ORIGOA_TEST_REPOSITORY")
	if root == "" {
		return
	}
	repo, err := Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	startAt, err := time.Parse(time.RFC3339Nano, os.Getenv("ORIGOA_TEST_START_AT"))
	if err != nil {
		t.Fatal(err)
	}
	if wait := time.Until(startAt); wait > 0 {
		time.Sleep(wait)
	}
	_, err = repo.Create(context.Background(), CreateInput{
		Kind: Entry, Type: "note", Title: os.Getenv("ORIGOA_TEST_TITLE"), HID: "PROCESS-UNIQUE",
	})
	var repoError *Error
	if err != nil && (!errors.As(err, &repoError) || repoError.Code != "duplicate_hid") {
		t.Fatal(err)
	}
}

func TestConcurrentOpenSameEmptyRepository(t *testing.T) {
	root := t.TempDir()
	start := make(chan struct{})
	errorsSeen := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			_, err := Open(context.Background(), root)
			errorsSeen <- err
		}()
	}
	close(start)
	for range 2 {
		if err := <-errorsSeen; err != nil {
			t.Fatalf("concurrent open failed: %v", err)
		}
	}
}

func TestOpenLinkedGitWorktree(t *testing.T) {
	parent := t.TempDir()
	mainRoot := filepath.Join(parent, "main")
	repo, err := Open(context.Background(), mainRoot)
	if err != nil {
		t.Fatal(err)
	}
	linkedRoot := filepath.Join(parent, "linked")
	if _, err := repo.git(context.Background(), "worktree", "add", "--quiet", "--detach", linkedRoot, "HEAD"); err != nil {
		t.Fatal(err)
	}
	linked, err := Open(context.Background(), linkedRoot)
	if err != nil {
		t.Fatal(err)
	}
	if linked.lockFile != repo.lockFile {
		t.Fatalf("linked worktrees use different locks: %q and %q", repo.lockFile, linked.lockFile)
	}
	if status, err := linked.git(context.Background(), "status", "--porcelain"); err != nil || status != "" {
		t.Fatalf("linked worktree is dirty: %q, %v", status, err)
	}
}

func TestConcurrentReadModifyWriteIsLossless(t *testing.T) {
	repo, _ := testRepository(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	item, err := repo.Create(ctx, CreateInput{Kind: Entry, Type: "note", Title: "Counter", Fields: map[string]any{"counter": 0}})
	if err != nil {
		t.Fatal(err)
	}

	const workers, increments = 8, 3
	var wait sync.WaitGroup
	var invalidReads atomic.Int64
	for range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for range increments {
				for {
					if ctx.Err() != nil {
						return
					}
					current, err := repo.Get(item.Artifact.GUID)
					if err != nil {
						invalidReads.Add(1)
						continue
					}
					counter, _ := numberValue(current.Artifact.Fields["counter"])
					_, err = repo.Update(ctx, item.Artifact.GUID, map[string]any{"fields": map[string]any{"counter": counter + 1}}, current.ETag)
					if err == nil {
						break
					}
					var repoError *Error
					if !errors.As(err, &repoError) || repoError.Code != "version_conflict" {
						invalidReads.Add(1)
						break
					}
				}
			}
		}()
	}
	wait.Wait()
	if ctx.Err() != nil {
		t.Fatal(ctx.Err())
	}
	if invalidReads.Load() != 0 {
		t.Fatalf("observed %d invalid transient states", invalidReads.Load())
	}
	final, err := repo.Get(item.Artifact.GUID)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := numberValue(final.Artifact.Fields["counter"]); !ok || got != float64(workers*increments) {
		t.Fatalf("lost update: counter is %v, want %d", got, workers*increments)
	}
}

func TestReadersNeverObservePartialCommits(t *testing.T) {
	repo, ctx := testRepository(t)
	item, err := repo.Create(ctx, CreateInput{Kind: Entry, Type: "note", Title: "Version 0", Fields: map[string]any{"nested": map[string]any{"stable": true}}})
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	var wait sync.WaitGroup
	var failures atomic.Int64
	for range 32 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				got, getErr := repo.Get(item.Artifact.GUID)
				if getErr != nil || got.Artifact.GUID != item.Artifact.GUID || got.ETag == "" || got.Artifact.Fields["nested"].(map[string]any)["stable"] != true {
					failures.Add(1)
					return
				}
				items, listErr := repo.List(Filters{})
				if listErr != nil || len(items) != 1 || items[0].Artifact.GUID != item.Artifact.GUID {
					failures.Add(1)
					return
				}
			}
		}()
	}

	current := item
	for version := 1; version <= 10; version++ {
		current, err = repo.Update(ctx, item.Artifact.GUID, map[string]any{"title": fmt.Sprintf("Version %d", version)}, current.ETag)
		if err != nil {
			close(done)
			wait.Wait()
			t.Fatal(err)
		}
	}
	close(done)
	wait.Wait()
	if failures.Load() != 0 {
		t.Fatalf("%d readers observed a partial or corrupt commit", failures.Load())
	}
}

func TestWriteLockHonorsCancellationAcrossHandles(t *testing.T) {
	root := t.TempDir()
	first, err := Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := first.lockWrite(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if _, err := second.lockWrite(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("contended lock ignored cancellation: %v", err)
	}
	unlock()
	canceled, cancelImmediately := context.WithCancel(context.Background())
	cancelImmediately()
	if _, err := second.lockWrite(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("free lock ignored prior cancellation: %v", err)
	}
}

func TestCachedResultsCannotBeMutatedByCallers(t *testing.T) {
	repo, ctx := testRepository(t)
	item, err := repo.Create(ctx, CreateInput{Kind: Entry, Type: "note", Title: "Immutable", Fields: map[string]any{"nested": map[string]any{"value": "original"}}})
	if err != nil {
		t.Fatal(err)
	}

	const readers = 32
	var wait sync.WaitGroup
	for index := range readers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			got, err := repo.Get(item.Artifact.GUID)
			if err != nil {
				t.Error(err)
				return
			}
			got.Artifact.Fields["nested"].(map[string]any)["value"] = index
		}()
	}
	wait.Wait()
	got, err := repo.Get(item.Artifact.GUID)
	if err != nil {
		t.Fatal(err)
	}
	if value := got.Artifact.Fields["nested"].(map[string]any)["value"]; value != "original" {
		t.Fatalf("caller mutated cached snapshot: %v", value)
	}
}

func TestCorruptOverlayCycleIsBounded(t *testing.T) {
	repo, ctx := testRepository(t)
	first, err := repo.Create(ctx, CreateInput{Kind: Entry, Type: "note", Title: "First"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := repo.Create(ctx, CreateInput{Kind: Entry, Type: "note", Title: "Second"})
	if err != nil {
		t.Fatal(err)
	}
	all := mustScan(t, repo)
	firstStored := all[strings.ToLower(first.Artifact.GUID)]
	secondStored := all[strings.ToLower(second.Artifact.GUID)]
	firstStored.Artifact.Base = second.Artifact.GUID
	secondStored.Artifact.Base = first.Artifact.GUID
	for _, stored := range []StoredArtifact{firstStored, secondStored} {
		raw, marshalErr := marshal(stored.Artifact)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if err := os.WriteFile(stored.file, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := repo.git(ctx, "add", "--all"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.git(ctx, "commit", "--quiet", "-m", "Inject overlay cycle"); err != nil {
		t.Fatal(err)
	}
	all = mustScan(t, repo)
	_, err = resolvedFields(all[strings.ToLower(first.Artifact.GUID)].Artifact, all)
	assertRepoError(t, err, 409, "overlay_cycle")
}

func TestExternalCommitInvalidatesSnapshot(t *testing.T) {
	repo, ctx := testRepository(t)
	item, err := repo.Create(ctx, CreateInput{Kind: Entry, Type: "note", Title: "Before"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Get(item.Artifact.GUID); err != nil {
		t.Fatal(err)
	}
	all := mustScan(t, repo)
	stored := all[strings.ToLower(item.Artifact.GUID)]
	stored.Artifact.Title = "External"
	raw, err := marshal(stored.Artifact)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stored.file, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.git(ctx, "add", "--", relative(repo.root, stored.file)); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.git(ctx, "commit", "--quiet", "-m", "External update", "--", relative(repo.root, stored.file)); err != nil {
		t.Fatal(err)
	}
	got, err := repo.Get(item.Artifact.GUID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Artifact.Title != "External" {
		t.Fatalf("stale snapshot returned %q", got.Artifact.Title)
	}
}

func TestOversizedManagedBlobIsRejectedBeforeDecode(t *testing.T) {
	repo, ctx := testRepository(t)
	item, err := repo.Create(ctx, CreateInput{Kind: Entry, Type: "note", Title: "Small"})
	if err != nil {
		t.Fatal(err)
	}
	all := mustScan(t, repo)
	stored := all[strings.ToLower(item.Artifact.GUID)]
	stored.Artifact.Content = strings.Repeat("x", maxManagedFile)
	raw, err := json.Marshal(stored.Artifact)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stored.file, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.git(ctx, "add", "--", relative(repo.root, stored.file)); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.git(ctx, "commit", "--quiet", "-m", "Inject oversized blob", "--", relative(repo.root, stored.file)); err != nil {
		t.Fatal(err)
	}
	_, err = repo.Get(item.Artifact.GUID)
	assertRepoError(t, err, 500, "repository_too_large")
}

func TestGitTreeReadStopsAtByteLimit(t *testing.T) {
	repo, ctx := testRepository(t)
	if _, err := repo.Create(ctx, CreateInput{Kind: Entry, Type: "note", Title: "Bounded tree"}); err != nil {
		t.Fatal(err)
	}
	_, err := repo.gitBytesLimited(ctx, 32, "ls-tree", "-r", "-z", "HEAD")
	assertRepoError(t, err, 500, "repository_too_large")
}

func TestArtifactCannotBeCommittedLargerThanReadLimit(t *testing.T) {
	repo, ctx := testRepository(t)
	item, err := repo.Create(ctx, CreateInput{
		Kind: Entry, Type: "note", Title: "Bounded", Content: strings.Repeat("a", 500_000),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = repo.Update(ctx, item.Artifact.GUID, map[string]any{
		"fields": map[string]any{"large": strings.Repeat("b", 600_000)},
	}, item.ETag)
	assertRepoError(t, err, 413, "artifact_too_large")
	got, err := repo.Get(item.Artifact.GUID)
	if err != nil || got.ETag != item.ETag {
		t.Fatalf("rejected update changed committed artifact: %#v, %v", got, err)
	}
}

func TestLargeJSONIntegerSurvivesUnrelatedUpdate(t *testing.T) {
	repo, ctx := testRepository(t)
	item, err := repo.Create(ctx, CreateInput{
		Kind: Entry, Type: "note", Title: "Exact", Fields: map[string]any{"number": json.Number("9007199254740993")},
	})
	if err != nil {
		t.Fatal(err)
	}
	item, err = repo.Update(ctx, item.Artifact.GUID, map[string]any{"title": "Still exact"}, item.ETag)
	if err != nil {
		t.Fatal(err)
	}
	if value, ok := item.Artifact.Fields["number"].(json.Number); !ok || value.String() != "9007199254740993" {
		t.Fatalf("large integer was changed: %T(%v)", item.Artifact.Fields["number"], item.Artifact.Fields["number"])
	}
}

func TestInvalidSchemaAndMissingWorkflowFailClosed(t *testing.T) {
	t.Run("unknown field type", func(t *testing.T) {
		repo, ctx := testRepository(t)
		configure(t, repo, ctx, ".origoa/schemas/note.json", map[string]any{
			"id": "note", "fields": map[string]any{"owner": map[string]any{"type": "artifact-refrence"}},
		})
		_, err := repo.Create(ctx, CreateInput{Kind: Entry, Type: "note", Title: "Invalid"})
		assertRepoError(t, err, 500, "repository_corrupt")
	})

	t.Run("malformed field metadata", func(t *testing.T) {
		repo, ctx := testRepository(t)
		configure(t, repo, ctx, ".origoa/schemas/note.json", map[string]any{
			"id": "note", "fields": map[string]any{"priority": map[string]any{"type": 42, "required": "yes"}},
		})
		_, err := repo.Create(ctx, CreateInput{Kind: Entry, Type: "note", Title: "Invalid"})
		assertRepoError(t, err, 500, "repository_corrupt")
	})

	t.Run("missing workflow", func(t *testing.T) {
		repo, ctx := testRepository(t)
		configure(t, repo, ctx, ".origoa/schemas/note.json", map[string]any{
			"id": "note", "fields": map[string]any{}, "workflows": []string{"missing"},
		})
		_, err := repo.Create(ctx, CreateInput{Kind: Entry, Type: "note", Title: "Invalid"})
		assertRepoError(t, err, 500, "repository_corrupt")
	})

	t.Run("invalid workflow state", func(t *testing.T) {
		repo, ctx := testRepository(t)
		configure(t, repo, ctx, ".origoa/schemas/note.json", map[string]any{
			"id": "note", "fields": map[string]any{}, "workflows": []string{"review"},
		})
		configure(t, repo, ctx, ".origoa/workflows/review.json", map[string]any{
			"id": "review", "initial": "draft", "states": []string{"draft", ""},
			"transitions": []map[string]string{{"id": "approve", "from": "draft", "to": ""}},
		})
		_, err := repo.Create(ctx, CreateInput{Kind: Entry, Type: "note", Title: "Invalid"})
		assertRepoError(t, err, 500, "repository_corrupt")
	})
}

func TestFilesystemSyncFailuresRestoreWorktree(t *testing.T) {
	for _, operation := range []string{"update", "delete"} {
		t.Run(operation, func(t *testing.T) {
			repo, ctx := testRepository(t)
			item, err := repo.Create(ctx, CreateInput{Kind: Entry, Type: "note", Title: "Before"})
			if err != nil {
				t.Fatal(err)
			}
			stored := mustScan(t, repo)[strings.ToLower(item.Artifact.GUID)]
			original := bytes.Clone(stored.raw)
			originalSync := syncDirectory
			calls := 0
			syncDirectory = func(path string) error {
				calls++
				if calls == 1 {
					return errors.New("injected directory sync failure")
				}
				return originalSync(path)
			}
			defer func() { syncDirectory = originalSync }()

			if operation == "update" {
				_, err = repo.Update(ctx, item.Artifact.GUID, map[string]any{"title": "After"}, item.ETag)
			} else {
				err = repo.Delete(ctx, item.Artifact.GUID, item.ETag)
			}
			if err == nil {
				t.Fatal("injected sync failure was ignored")
			}
			if raw := mustRead(t, stored.file); !bytes.Equal(raw, original) {
				t.Fatalf("failed %s changed worktree", operation)
			}
			if status, statusErr := repo.git(ctx, "status", "--porcelain"); statusErr != nil || status != "" {
				t.Fatalf("failed %s left dirty state: %q, %v", operation, status, statusErr)
			}
		})
	}
}

func TestCommittedArtifactInHiddenFolderIsRejected(t *testing.T) {
	repo, ctx := testRepository(t)
	item, err := repo.Create(ctx, CreateInput{Kind: Entry, Type: "note", Title: "Hidden"})
	if err != nil {
		t.Fatal(err)
	}
	from := pathpkg.Join(item.Path, item.Artifact.GUID)
	to := pathpkg.Join(".origoa", item.Artifact.GUID)
	if _, err := repo.git(ctx, "mv", from, to); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.git(ctx, "commit", "--quiet", "-m", "Inject hidden artifact", "--", from, to); err != nil {
		t.Fatal(err)
	}
	_, err = repo.Get(item.Artifact.GUID)
	assertRepoError(t, err, 500, "repository_corrupt")
}

func TestAmbiguousSuccessfulGitCommitIsReportedAsSuccess(t *testing.T) {
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}

	for _, operation := range []string{"create", "update", "delete"} {
		t.Run(operation, func(t *testing.T) {
			repo, ctx := testRepository(t)
			var item StoredArtifact
			if operation != "create" {
				item, err = repo.Create(ctx, CreateInput{Kind: Entry, Type: "note", Title: "Before"})
				if err != nil {
					t.Fatal(err)
				}
			}
			restore := failAfterSuccessfulCommit(t, realGit)
			defer restore()
			switch operation {
			case "create":
				_, err = repo.Create(ctx, CreateInput{Kind: Entry, Type: "note", Title: "Created"})
			case "update":
				_, err = repo.Update(ctx, item.Artifact.GUID, map[string]any{"title": "After"}, item.ETag)
			case "delete":
				err = repo.Delete(ctx, item.Artifact.GUID, item.ETag)
			}
			if err != nil {
				t.Fatalf("successful %s was reported as failure: %v", operation, err)
			}
		})
	}
}

func TestFailedGitCommitRollsBackEveryMutation(t *testing.T) {
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}

	for _, operation := range []string{"create", "update", "delete"} {
		t.Run(operation, func(t *testing.T) {
			repo, ctx := testRepository(t)
			var item StoredArtifact
			if operation != "create" {
				item, err = repo.Create(ctx, CreateInput{Kind: Entry, Type: "note", Title: "Before"})
				if err != nil {
					t.Fatal(err)
				}
			}
			restore := failBeforeCommit(t, realGit)
			defer restore()
			switch operation {
			case "create":
				_, err = repo.Create(ctx, CreateInput{Kind: Entry, Type: "note", Title: "Created"})
			case "update":
				_, err = repo.Update(ctx, item.Artifact.GUID, map[string]any{"title": "After"}, item.ETag)
			case "delete":
				err = repo.Delete(ctx, item.Artifact.GUID, item.ETag)
			}
			if err == nil {
				t.Fatalf("failed %s was reported as success", operation)
			}

			items, listErr := repo.List(Filters{})
			if listErr != nil {
				t.Fatal(listErr)
			}
			if operation == "create" {
				if len(items) != 0 {
					t.Fatalf("failed create remained visible: %#v", items)
				}
			} else if len(items) != 1 || items[0].ETag != item.ETag || items[0].Artifact.Title != "Before" {
				t.Fatalf("failed %s changed committed state: %#v", operation, items)
			}
			if status, statusErr := repo.git(ctx, "status", "--porcelain"); statusErr != nil || status != "" {
				t.Fatalf("failed %s left a dirty worktree: %q, %v", operation, status, statusErr)
			}
		})
	}
}

func failBeforeCommit(t *testing.T, realGit string) func() {
	t.Helper()
	return installGitWrapper(t, realGit, `#!/bin/sh
for argument in "$@"; do
  [ "$argument" = commit ] && exit 87
done
exec "$ORIGOA_REAL_GIT" "$@"
`)
}

func failAfterSuccessfulCommit(t *testing.T, realGit string) func() {
	t.Helper()
	return installGitWrapper(t, realGit, `#!/bin/sh
for argument in "$@"; do
  if [ "$argument" = commit ]; then
    "$ORIGOA_REAL_GIT" "$@"
    status=$?
    [ "$status" -ne 0 ] && exit "$status"
    exit 86
  fi
done
exec "$ORIGOA_REAL_GIT" "$@"
`)
}

func installGitWrapper(t *testing.T, realGit, content string) func() {
	t.Helper()
	directory := t.TempDir()
	script := filepath.Join(directory, "git")
	if err := os.WriteFile(script, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
	originalPath := os.Getenv("PATH")
	originalGit := os.Getenv("ORIGOA_REAL_GIT")
	if err := os.Setenv("ORIGOA_REAL_GIT", realGit); err != nil {
		t.Fatal(err)
	}
	if err := os.Setenv("PATH", directory+string(os.PathListSeparator)+originalPath); err != nil {
		t.Fatal(err)
	}
	return func() {
		_ = os.Setenv("PATH", originalPath)
		_ = os.Setenv("ORIGOA_REAL_GIT", originalGit)
	}
}

func FuzzSafeFolder(f *testing.F) {
	for _, seed := range []string{"artifacts", "a/b", "../escape", "/absolute", ".git", "a\\b", "a//b", "é/文档", string([]byte{'a', 0, 'b'})} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		folder, err := safeFolder(value)
		if err != nil {
			return
		}
		if folder == "" || filepath.IsAbs(folder) || strings.Contains(folder, "\\") {
			t.Fatalf("unsafe accepted folder %q -> %q", value, folder)
		}
		for _, part := range strings.Split(folder, "/") {
			if part == "." || part == ".." || strings.HasPrefix(part, ".") {
				t.Fatalf("unsafe accepted folder %q -> %q", value, folder)
			}
		}
	})
}

func FuzzStrictArtifactDecoder(f *testing.F) {
	f.Add([]byte(`{"guid":"00000000-0000-4000-8000-000000000000","kind":"entry","type":"note","title":"x"}`))
	f.Add([]byte(`{"unknown":true}`))
	f.Add([]byte{0xff, 0x00, '{'})
	f.Fuzz(func(t *testing.T, raw []byte) {
		var artifact Artifact
		_ = decodeStrict(raw, &artifact)
	})
}

func FuzzApplyPatch(f *testing.F) {
	f.Add([]byte(`{"title":"updated"}`))
	f.Add([]byte(`{"fields":{"nested":[1,true,null]}}`))
	f.Add([]byte(`{"__proto__":{"admin":true}}`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		var patch map[string]any
		if json.Unmarshal(raw, &patch) != nil {
			return
		}
		artifact := Artifact{GUID: "00000000-0000-4000-8000-000000000000", Kind: Entry, Type: "note", Title: "x"}
		_ = applyPatch(&artifact, patch)
	})
}

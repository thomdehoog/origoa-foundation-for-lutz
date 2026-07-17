// Package gitx manages a bare Git repository through plumbing commands.
//
// Git is the authoritative store. Commits are constructed without a working
// directory (hash-object, update-index against a private index file,
// write-tree, commit-tree) and published with a compare-and-swap update-ref,
// retrying on concurrent updates.
package gitx

import (
	"bytes"
	"crypto/sha1"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const Branch = "refs/heads/main"

type Repo struct {
	Dir string
}

// Op is a single file change within a commit: either a write or a delete.
type Op struct {
	Path    string
	Content []byte
	Delete  bool
}

// TreeEntry is one blob in the repository tree.
type TreeEntry struct {
	Path string
	SHA  string
}

// Init opens dir as a bare repository, creating it if necessary.
func Init(dir string) (*Repo, error) {
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); os.IsNotExist(err) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		cmd := exec.Command("git", "init", "--bare", "-b", "main", dir)
		if out, err := cmd.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("git init: %v: %s", err, out)
		}
	}
	return &Repo{Dir: dir}, nil
}

func (r *Repo) git(stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Env = append(os.Environ(),
		"GIT_DIR="+r.Dir,
		"GIT_AUTHOR_NAME=origoa", "GIT_AUTHOR_EMAIL=origoa@localhost",
		"GIT_COMMITTER_NAME=origoa", "GIT_COMMITTER_EMAIL=origoa@localhost",
	)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git %s: %v: %s", args[0], err, errb.String())
	}
	return out.Bytes(), nil
}

// Head returns the current commit of the main branch, or "" if unborn.
func (r *Repo) Head() (string, error) {
	out, err := r.git(nil, "rev-parse", "--verify", "--quiet", Branch)
	if err != nil {
		return "", nil // unborn branch
	}
	return strings.TrimSpace(string(out)), nil
}

// ReadBlob returns the content of path at rev.
func (r *Repo) ReadBlob(rev, path string) ([]byte, error) {
	return r.git(nil, "cat-file", "blob", rev+":"+path)
}

// ListTree returns every blob under prefix ("" for the whole tree) at rev.
func (r *Repo) ListTree(rev, prefix string) ([]TreeEntry, error) {
	if rev == "" {
		return nil, nil
	}
	args := []string{"ls-tree", "-r", "-z", "--format=%(objectname) %(path)", rev}
	if prefix != "" {
		args = append(args, "--", prefix)
	}
	out, err := r.git(nil, args...)
	if err != nil {
		return nil, err
	}
	var entries []TreeEntry
	for _, line := range strings.Split(string(out), "\x00") {
		if line == "" {
			continue
		}
		sha, path, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		entries = append(entries, TreeEntry{Path: path, SHA: sha})
	}
	return entries, nil
}

// ReadBlobs fetches many blobs by SHA in a single cat-file --batch call.
func (r *Repo) ReadBlobs(shas []string) (map[string][]byte, error) {
	if len(shas) == 0 {
		return map[string][]byte{}, nil
	}
	stdin := strings.Join(shas, "\n") + "\n"
	out, err := r.git([]byte(stdin), "cat-file", "--batch")
	if err != nil {
		return nil, err
	}
	res := make(map[string][]byte, len(shas))
	buf := out
	for len(buf) > 0 {
		nl := bytes.IndexByte(buf, '\n')
		if nl < 0 {
			break
		}
		header := string(buf[:nl])
		buf = buf[nl+1:]
		parts := strings.Fields(header)
		if len(parts) < 3 || parts[1] != "blob" {
			return nil, fmt.Errorf("gitx: unexpected batch header %q", header)
		}
		var size int
		fmt.Sscanf(parts[2], "%d", &size)
		if size > len(buf) {
			return nil, fmt.Errorf("gitx: truncated batch output")
		}
		res[parts[0]] = append([]byte(nil), buf[:size]...)
		buf = buf[size:]
		if len(buf) > 0 && buf[0] == '\n' {
			buf = buf[1:]
		}
	}
	return res, nil
}

// Log returns commit history for path (whole repo if path == "").
// Each entry is hash, author date (ISO), subject.
type LogEntry struct {
	Hash    string `json:"hash"`
	Date    string `json:"date"`
	Subject string `json:"subject"`
}

func (r *Repo) Log(path string, limit int) ([]LogEntry, error) {
	head, err := r.Head()
	if err != nil || head == "" {
		return nil, err
	}
	args := []string{"log", fmt.Sprintf("--max-count=%d", limit), "--format=%H%x1f%aI%x1f%s%x1e", head}
	if path != "" {
		args = append(args, "--", path)
	}
	out, err := r.git(nil, args...)
	if err != nil {
		return nil, err
	}
	var log []LogEntry
	for _, rec := range strings.Split(string(out), "\x1e") {
		rec = strings.TrimSpace(rec)
		if rec == "" {
			continue
		}
		f := strings.Split(rec, "\x1f")
		if len(f) == 3 {
			log = append(log, LogEntry{Hash: f[0], Date: f[1], Subject: f[2]})
		}
	}
	return log, nil
}

// BlobSHA computes the Git blob object name for content without touching the
// repository. Used as the optimistic-concurrency ETag.
func BlobSHA(content []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(content))
	h.Write(content)
	return fmt.Sprintf("%x", h.Sum(nil))
}

// Commit applies ops on top of the current branch head and publishes the
// result with a compare-and-swap ref update. On a concurrent update it
// rebuilds the commit on the new head and retries.
func (r *Repo) Commit(message string, ops []Op) (string, error) {
	for attempt := 0; attempt < 8; attempt++ {
		head, err := r.Head()
		if err != nil {
			return "", err
		}
		commit, err := r.buildCommit(head, message, ops)
		if err != nil {
			return "", err
		}
		// CAS publish: old value "" asserts the ref must not exist yet.
		var casErr error
		if head == "" {
			_, casErr = r.git(nil, "update-ref", Branch, commit, "")
		} else {
			_, casErr = r.git(nil, "update-ref", Branch, commit, head)
		}
		if casErr == nil {
			return commit, nil
		}
	}
	return "", fmt.Errorf("gitx: commit failed after retries (concurrent updates)")
}

func (r *Repo) buildCommit(head, message string, ops []Op) (string, error) {
	idx, err := os.CreateTemp("", "origoa-index-*")
	if err != nil {
		return "", err
	}
	idx.Close()
	defer os.Remove(idx.Name())
	withIdx := func(stdin []byte, args ...string) ([]byte, error) {
		cmd := exec.Command("git", args...)
		cmd.Env = append(os.Environ(),
			"GIT_DIR="+r.Dir, "GIT_INDEX_FILE="+idx.Name(),
			"GIT_AUTHOR_NAME=origoa", "GIT_AUTHOR_EMAIL=origoa@localhost",
			"GIT_COMMITTER_NAME=origoa", "GIT_COMMITTER_EMAIL=origoa@localhost",
		)
		if stdin != nil {
			cmd.Stdin = bytes.NewReader(stdin)
		}
		var out, errb bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &errb
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("git %s: %v: %s", args[0], err, errb.String())
		}
		return out.Bytes(), nil
	}

	if head != "" {
		if _, err := withIdx(nil, "read-tree", head); err != nil {
			return "", err
		}
	} else {
		if _, err := withIdx(nil, "read-tree", "--empty"); err != nil {
			return "", err
		}
	}
	for _, op := range ops {
		if op.Delete {
			// Mode 0 via --index-info removes the entry; works without a work tree.
			line := fmt.Sprintf("0 %040d\t%s\n", 0, op.Path)
			if _, err := withIdx([]byte(line), "update-index", "--index-info"); err != nil {
				return "", err
			}
			continue
		}
		sha, err := withIdx(op.Content, "hash-object", "-w", "--stdin")
		if err != nil {
			return "", err
		}
		info := fmt.Sprintf("100644,%s,%s", strings.TrimSpace(string(sha)), op.Path)
		if _, err := withIdx(nil, "update-index", "--add", "--cacheinfo", info); err != nil {
			return "", err
		}
	}
	tree, err := withIdx(nil, "write-tree")
	if err != nil {
		return "", err
	}
	args := []string{"commit-tree", strings.TrimSpace(string(tree)), "-m", message}
	if head != "" {
		args = append(args, "-p", head)
	}
	commit, err := withIdx(nil, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(commit)), nil
}

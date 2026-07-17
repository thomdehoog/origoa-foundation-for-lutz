package repository

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"math"
	"math/big"
	"net/url"
	"os"
	"os/exec"
	pathpkg "path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

var (
	uuidPattern       = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	hashPattern       = regexp.MustCompile(`^[0-9a-f]{40,64}$`)
	identifierPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9._-]{0,63}$`)
	hidPattern        = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	pathPartPattern   = regexp.MustCompile(`^[\pL\pN][\pL\pN._ -]{0,63}$`)
)

const (
	maxContentLength  = 500_000
	maxManagedFile    = 1 << 20
	maxManagedFiles   = 100_000
	maxManagedContent = 256 << 20
	maxTreeListing    = 64 << 20
)

type Kind string

const (
	Entry    Kind = "entry"
	Document Kind = "document"
	Link     Kind = "link"
	Comment  Kind = "comment"
)

type Artifact struct {
	GUID      string            `json:"guid"`
	Kind      Kind              `json:"kind"`
	Type      string            `json:"type"`
	Title     string            `json:"title"`
	HID       string            `json:"hid,omitempty"`
	Base      string            `json:"base,omitempty"`
	Fields    map[string]any    `json:"fields,omitempty"`
	Content   any               `json:"content,omitempty"`
	Source    string            `json:"source,omitempty"`
	Target    string            `json:"target,omitempty"`
	LinkType  string            `json:"linkType,omitempty"`
	Subject   string            `json:"subject,omitempty"`
	Parent    string            `json:"parent,omitempty"`
	Body      string            `json:"body,omitempty"`
	Workflows map[string]string `json:"workflows,omitempty"`
}

type StoredArtifact struct {
	Artifact Artifact `json:"artifact"`
	ETag     string   `json:"etag"`
	Path     string   `json:"path"`
	file     string
	raw      []byte
}

type CreateInput struct {
	Kind     Kind           `json:"kind"`
	Type     string         `json:"type"`
	Title    string         `json:"title"`
	Path     string         `json:"path,omitempty"`
	HID      string         `json:"hid,omitempty"`
	Base     string         `json:"base,omitempty"`
	Fields   map[string]any `json:"fields,omitempty"`
	Content  any            `json:"content,omitempty"`
	Source   string         `json:"source,omitempty"`
	Target   string         `json:"target,omitempty"`
	LinkType string         `json:"linkType,omitempty"`
	Subject  string         `json:"subject,omitempty"`
	Parent   string         `json:"parent,omitempty"`
	Body     string         `json:"body,omitempty"`
}

type Filters struct {
	Kind Kind
	Type string
	Path string
}

type SearchInput struct {
	Query string
	Kind  Kind
	Type  string
	Limit int
}

type Links struct {
	Incoming []StoredArtifact `json:"incoming"`
	Outgoing []StoredArtifact `json:"outgoing"`
}

type Overlay struct {
	Artifact Artifact `json:"artifact"`
	Chain    []string `json:"chain"`
}

type HistoryItem struct {
	Commit  string `json:"commit"`
	Date    string `json:"date"`
	Author  string `json:"author"`
	Subject string `json:"subject"`
}

type WorkflowInfo struct {
	ID          string               `json:"id"`
	State       string               `json:"state"`
	Transitions []WorkflowTransition `json:"transitions"`
}

type WorkflowTransition struct {
	ID    string `json:"id"`
	From  string `json:"from"`
	To    string `json:"to"`
	Label string `json:"label,omitempty"`
}

type workflow struct {
	ID          string               `json:"id"`
	Initial     string               `json:"initial"`
	States      []string             `json:"states"`
	Transitions []WorkflowTransition `json:"transitions"`
}

type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

func fail(status int, code, message string) error {
	return &Error{Status: status, Code: code, Message: message}
}

type Repository struct {
	root      string
	gitDir    string
	lockFile  string
	cacheMu   sync.RWMutex
	rebuildMu sync.Mutex
	cache     *repositorySnapshot
}

type repositorySnapshot struct {
	revision  string
	artifacts map[string]StoredArtifact
	files     map[string][]byte
}

type treeEntry struct {
	name string
	hash string
}

func Open(ctx context.Context, requestedRoot string) (*Repository, error) {
	root, err := filepath.Abs(requestedRoot)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	r := &Repository{root: root}
	initializationLock := filepath.Join(root, ".origoa-initialize.lock")
	unlock, err := lockFile(ctx, initializationLock)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := r.initialize(ctx); err != nil {
		return nil, err
	}
	gitDir, err := r.git(ctx, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return nil, err
	}
	r.gitDir = gitDir
	commonDir, err := r.git(ctx, "rev-parse", "--git-common-dir")
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(root, commonDir)
	}
	commonDir = filepath.Clean(commonDir)
	r.lockFile = filepath.Join(commonDir, "origoa.lock")
	if err := excludeInitializationLock(commonDir); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *Repository) Root() string { return r.root }

func (r *Repository) initialize(ctx context.Context) error {
	if _, err := os.Stat(filepath.Join(r.root, ".git")); errors.Is(err, fs.ErrNotExist) {
		if _, err := r.git(ctx, "init", "--quiet", "--initial-branch=main"); err != nil {
			return err
		}
	}
	if value, _ := r.git(ctx, "config", "--get", "user.name"); value == "" {
		if _, err := r.git(ctx, "config", "user.name", "Origoa Foundation"); err != nil {
			return err
		}
	}
	if value, _ := r.git(ctx, "config", "--get", "user.email"); value == "" {
		if _, err := r.git(ctx, "config", "user.email", "origoa@localhost"); err != nil {
			return err
		}
	}
	configFile := filepath.Join(r.root, ".origoa", "config.json")
	if _, committed, err := r.headFile(ctx, ".origoa/config.json"); err != nil {
		return err
	} else if !committed {
		if err := os.MkdirAll(filepath.Dir(configFile), 0o700); err != nil {
			return err
		}
		if _, err := os.Stat(configFile); errors.Is(err, fs.ErrNotExist) {
			config := []byte("{\n  \"guidFiles\": [\"artifact.json\"],\n  \"configFolders\": [\".origoa\"],\n  \"indexers\": [\"foundation\"]\n}\n")
			if err := os.WriteFile(configFile, config, 0o600); err != nil {
				return err
			}
		}
		if _, err := r.git(ctx, "add", "--", ".origoa/config.json"); err != nil {
			return err
		}
		if _, err := r.git(ctx, "commit", "--quiet", "-m", "Initialize Origoa repository", "--", ".origoa/config.json"); err != nil {
			return err
		}
	}
	return nil
}

func (r *Repository) git(ctx context.Context, args ...string) (string, error) {
	output, err := r.gitBytes(ctx, args...)
	return strings.TrimSpace(string(output)), err
}

func (r *Repository) gitBytes(ctx context.Context, args ...string) ([]byte, error) {
	return r.gitInputBytes(ctx, nil, args...)
}

func (r *Repository) gitInputBytes(ctx context.Context, input []byte, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "git", append([]string{"-C", r.root}, args...)...)
	command.Env = append(os.Environ(), "LC_ALL=C")
	command.Stdin = bytes.NewReader(input)
	output, err := command.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

func (r *Repository) gitBytesLimited(ctx context.Context, limit int64, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "git", append([]string{"-C", r.root}, args...)...)
	command.Env = append(os.Environ(), "LC_ALL=C")
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		return nil, err
	}
	output, readErr := io.ReadAll(io.LimitReader(stdout, limit+1))
	if readErr != nil || int64(len(output)) > limit {
		_ = command.Process.Kill()
		_ = command.Wait()
		if readErr != nil {
			return nil, readErr
		}
		return nil, fail(500, "repository_too_large", "Managed repository tree exceeds the size limit.")
	}
	if err := command.Wait(); err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return output, nil
}

func (r *Repository) headFile(ctx context.Context, name string) ([]byte, bool, error) {
	object := "HEAD:" + name
	output, err := r.gitInputBytes(ctx, []byte(object+"\n"), "cat-file", "--batch")
	if err != nil {
		return nil, false, err
	}
	reader := bufio.NewReader(bytes.NewReader(output))
	header, err := reader.ReadString('\n')
	if err != nil {
		return nil, false, fmt.Errorf("read git object header: %w", err)
	}
	if strings.HasSuffix(strings.TrimSpace(header), " missing") {
		return nil, false, nil
	}
	fields := strings.Fields(header)
	if len(fields) != 3 || fields[1] != "blob" {
		return nil, false, errors.New("unexpected git object header")
	}
	size, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil || size < 0 || size > maxManagedFile {
		return nil, false, errors.New("git object exceeds managed file limit")
	}
	raw := make([]byte, size)
	if _, err := io.ReadFull(reader, raw); err != nil {
		return nil, false, fmt.Errorf("read git object: %w", err)
	}
	return raw, true, nil
}

func (r *Repository) lockWrite(ctx context.Context) (func(), error) {
	return lockFile(ctx, r.lockFile)
}

func excludeInitializationLock(gitDir string) error {
	name := filepath.Join(gitDir, "info", "exclude")
	raw, err := os.ReadFile(name)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "/.origoa-initialize.lock" {
			return nil
		}
	}
	file, err := os.OpenFile(name, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if len(raw) > 0 && raw[len(raw)-1] != '\n' {
		_, err = file.WriteString("\n")
	}
	if err == nil {
		_, err = file.WriteString("/.origoa-initialize.lock\n")
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	return err
}

func lockFile(ctx context.Context, name string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(name, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		if err := ctx.Err(); err != nil {
			_ = file.Close()
			return nil, err
		}
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
				_ = file.Close()
			}, nil
		}
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			_ = file.Close()
			return nil, err
		}
		timer := time.NewTimer(5 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			_ = file.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func kindValid(kind Kind) bool {
	return kind == Entry || kind == Document || kind == Link || kind == Comment
}

func validateGUID(value, label string) error {
	if !uuidPattern.MatchString(value) {
		return fail(400, "invalid_guid", label+" must be a UUID.")
	}
	return nil
}

func validateIdentifier(value, label string) error {
	if !identifierPattern.MatchString(value) {
		return fail(400, "invalid_identifier", label+" is invalid.")
	}
	return nil
}

func safeFolder(value string) (string, error) {
	if value == "" {
		value = "artifacts"
	}
	if len(value) > 256 || filepath.IsAbs(value) || strings.Contains(value, "\\") {
		return "", fail(400, "invalid_path", "Path is invalid.")
	}
	clean := filepath.ToSlash(filepath.Clean(value))
	parts := strings.Split(clean, "/")
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fail(400, "invalid_path", "Path is invalid.")
	}
	for _, part := range parts {
		if strings.HasPrefix(part, ".") || !pathPartPattern.MatchString(part) {
			return "", fail(400, "invalid_path", "Path is invalid.")
		}
	}
	return clean, nil
}

func checksum(raw []byte) string {
	sum := sha256.Sum256(raw)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func marshal(value any) ([]byte, error) {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, fail(400, "invalid_json", "Artifact contains unsupported JSON values.")
	}
	return append(raw, '\n'), nil
}

func marshalArtifact(artifact Artifact) ([]byte, error) {
	raw, err := marshal(artifact)
	if err == nil && len(raw) > maxManagedFile {
		return nil, fail(413, "artifact_too_large", "Serialized artifact exceeds the size limit.")
	}
	return raw, err
}

func clone[T any](value T) (T, error) {
	var result T
	raw, err := json.Marshal(value)
	if err == nil {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		err = decoder.Decode(&result)
	}
	return result, err
}

func (r *Repository) scan() (map[string]StoredArtifact, error) {
	ctx := context.Background()
	snapshot, err := r.snapshot(ctx)
	if err != nil {
		return nil, err
	}
	return cloneArtifacts(snapshot.artifacts)
}

func (r *Repository) snapshot(ctx context.Context) (*repositorySnapshot, error) {
	revision, err := r.currentRevision(ctx)
	if err != nil {
		return nil, err
	}
	r.cacheMu.RLock()
	cached := r.cache
	r.cacheMu.RUnlock()
	if cached != nil && cached.revision == revision {
		return cached, nil
	}

	r.rebuildMu.Lock()
	defer r.rebuildMu.Unlock()
	r.cacheMu.RLock()
	cached = r.cache
	r.cacheMu.RUnlock()
	if cached != nil && cached.revision == revision {
		return cached, nil
	}

	built, err := r.buildSnapshot(ctx, revision)
	if err != nil {
		return nil, err
	}
	r.cacheMu.Lock()
	r.cache = built
	r.cacheMu.Unlock()
	return built, nil
}

func (r *Repository) currentRevision(ctx context.Context) (string, error) {
	head, err := os.ReadFile(filepath.Join(r.gitDir, "HEAD"))
	if err == nil {
		value := strings.TrimSpace(string(head))
		if hashPattern.MatchString(value) {
			return value, nil
		}
		if strings.HasPrefix(value, "ref: refs/") {
			name := strings.TrimPrefix(value, "ref: ")
			clean := filepath.Clean(filepath.FromSlash(name))
			if !filepath.IsAbs(clean) && clean != ".." && !strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
				if raw, readErr := os.ReadFile(filepath.Join(r.gitDir, clean)); readErr == nil {
					revision := strings.TrimSpace(string(raw))
					if hashPattern.MatchString(revision) {
						return revision, nil
					}
				}
			}
		}
	}
	return r.git(ctx, "rev-parse", "HEAD")
}

func (r *Repository) buildSnapshot(ctx context.Context, revision string) (*repositorySnapshot, error) {
	listing, err := r.gitBytesLimited(ctx, maxTreeListing, "ls-tree", "-r", "-z", revision)
	if err != nil {
		return nil, err
	}
	entries := []treeEntry{}
	for _, rawEntry := range bytes.Split(listing, []byte{0}) {
		parts := bytes.SplitN(rawEntry, []byte{'\t'}, 2)
		if len(parts) != 2 {
			continue
		}
		metadata := strings.Fields(string(parts[0]))
		name := string(parts[1])
		if len(metadata) != 3 || metadata[1] != "blob" || !managedFile(name) {
			continue
		}
		if metadata[0] == "120000" {
			return nil, fail(500, "repository_corrupt", "Managed file cannot be a symbolic link: "+name+".")
		}
		entries = append(entries, treeEntry{name: name, hash: metadata[2]})
	}
	if len(entries) > maxManagedFiles {
		return nil, fail(500, "repository_too_large", "Repository contains too many managed files.")
	}
	blobs, err := r.readBlobs(ctx, entries)
	if err != nil {
		return nil, err
	}

	snapshot := &repositorySnapshot{revision: revision, artifacts: map[string]StoredArtifact{}, files: blobs}
	for _, entry := range entries {
		if !artifactFile(entry.name) {
			continue
		}
		raw := blobs[entry.name]
		var artifact Artifact
		if err := decodeStrict(raw, &artifact); err != nil {
			return nil, fail(500, "repository_corrupt", "Invalid artifact in "+entry.name+": "+err.Error())
		}
		if err := validateArtifact(artifact); err != nil {
			return nil, fail(500, "repository_corrupt", "Invalid artifact in "+entry.name+": "+err.Error())
		}
		if !strings.EqualFold(pathpkg.Base(pathpkg.Dir(entry.name)), artifact.GUID) {
			return nil, fail(500, "repository_corrupt", "Artifact GUID does not match its directory.")
		}
		key := strings.ToLower(artifact.GUID)
		if _, duplicate := snapshot.artifacts[key]; duplicate {
			return nil, fail(500, "repository_corrupt", "Duplicate artifact GUID "+artifact.GUID+".")
		}
		relativePath := pathpkg.Dir(pathpkg.Dir(entry.name))
		if relativePath == "." {
			relativePath = ""
		}
		if safePath, err := safeFolder(relativePath); err != nil || safePath != relativePath {
			return nil, fail(500, "repository_corrupt", "Artifact is stored in an invalid folder: "+entry.name+".")
		}
		snapshot.artifacts[key] = StoredArtifact{
			Artifact: artifact,
			ETag:     checksum(raw),
			Path:     relativePath,
			file:     filepath.Join(r.root, filepath.FromSlash(entry.name)),
			raw:      raw,
		}
	}
	return snapshot, nil
}

func managedFile(name string) bool {
	return artifactFile(name) || strings.Contains(name, "/.origoa/schemas/") || strings.HasPrefix(name, ".origoa/schemas/") ||
		strings.Contains(name, "/.origoa/workflows/") || strings.HasPrefix(name, ".origoa/workflows/")
}

func artifactFile(name string) bool {
	return pathpkg.Base(name) == "artifact.json" && uuidPattern.MatchString(pathpkg.Base(pathpkg.Dir(name)))
}

func (r *Repository) readBlobs(ctx context.Context, entries []treeEntry) (map[string][]byte, error) {
	if len(entries) == 0 {
		return map[string][]byte{}, nil
	}
	hashes := make([]string, 0, len(entries))
	seen := map[string]bool{}
	for _, entry := range entries {
		if !seen[entry.hash] {
			hashes = append(hashes, entry.hash)
			seen[entry.hash] = true
		}
	}
	input := []byte(strings.Join(hashes, "\n") + "\n")
	checked, err := r.gitInputBytes(ctx, input, "cat-file", "--batch-check")
	if err != nil {
		return nil, err
	}
	total := int64(0)
	for _, line := range strings.Split(strings.TrimSpace(string(checked)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 || fields[1] != "blob" {
			return nil, fail(500, "repository_corrupt", "Managed Git object is not a blob.")
		}
		size, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil || size < 0 || size > maxManagedFile {
			return nil, fail(500, "repository_too_large", "Managed file exceeds the size limit.")
		}
		total += size
		if total > maxManagedContent {
			return nil, fail(500, "repository_too_large", "Managed repository content exceeds the memory limit.")
		}
	}
	batched, err := r.gitInputBytes(ctx, input, "cat-file", "--batch")
	if err != nil {
		return nil, err
	}
	reader := bufio.NewReader(bytes.NewReader(batched))
	byHash := map[string][]byte{}
	for range hashes {
		header, err := reader.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("read Git batch header: %w", err)
		}
		fields := strings.Fields(header)
		if len(fields) != 3 || fields[1] != "blob" {
			return nil, fail(500, "repository_corrupt", "Unexpected Git batch response.")
		}
		size, _ := strconv.Atoi(fields[2])
		content := make([]byte, size)
		if _, err := io.ReadFull(reader, content); err != nil {
			return nil, fmt.Errorf("read Git blob: %w", err)
		}
		if separator, err := reader.ReadByte(); err != nil || separator != '\n' {
			return nil, errors.New("invalid Git batch separator")
		}
		byHash[fields[0]] = content
	}
	files := make(map[string][]byte, len(entries))
	for _, entry := range entries {
		files[entry.name] = byHash[entry.hash]
	}
	return files, nil
}

func cloneArtifacts(source map[string]StoredArtifact) (map[string]StoredArtifact, error) {
	result := make(map[string]StoredArtifact, len(source))
	for key, item := range source {
		cloned, err := cloneStored(item)
		if err != nil {
			return nil, err
		}
		cloned.raw = bytes.Clone(item.raw)
		result[key] = cloned
	}
	return result, nil
}

func cloneStored(item StoredArtifact) (StoredArtifact, error) {
	artifact, err := clone(item.Artifact)
	if err != nil {
		return StoredArtifact{}, err
	}
	item.Artifact = artifact
	item.raw = nil
	return item, nil
}

func decodeStrict(raw []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("multiple JSON values")
	}
	return nil
}

func validateArtifact(artifact Artifact) error {
	if err := validateGUID(artifact.GUID, "GUID"); err != nil {
		return err
	}
	if !kindValid(artifact.Kind) {
		return fail(400, "invalid_kind", "Artifact kind is invalid.")
	}
	if err := validateIdentifier(artifact.Type, "Artifact type"); err != nil {
		return err
	}
	if strings.TrimSpace(artifact.Title) == "" || len(artifact.Title) > 300 {
		return fail(400, "invalid_title", "Title must be 1-300 characters.")
	}
	if artifact.HID != "" && !hidPattern.MatchString(artifact.HID) {
		return fail(400, "invalid_hid", "HID is invalid.")
	}
	for label, guid := range map[string]string{
		"base": artifact.Base, "source": artifact.Source, "target": artifact.Target,
		"subject": artifact.Subject, "parent": artifact.Parent,
	} {
		if guid != "" {
			if err := validateGUID(guid, label); err != nil {
				return err
			}
		}
	}
	if content, ok := artifact.Content.(string); ok && len(content) > maxContentLength {
		return fail(400, "content_too_large", "Document content is too large.")
	}
	if len(artifact.Body) > 100_000 {
		return fail(400, "invalid_body", "Comment body is too large.")
	}
	if artifact.Kind == Link {
		if artifact.Source == "" || artifact.Target == "" {
			return fail(400, "invalid_link", "Link source and target are required.")
		}
		if err := validateIdentifier(artifact.LinkType, "Link type"); err != nil {
			return err
		}
	}
	if artifact.Kind == Comment && (artifact.Subject == "" || strings.TrimSpace(artifact.Body) == "") {
		return fail(400, "invalid_comment", "Comment subject and body are required.")
	}
	raw, err := json.Marshal(artifact)
	if err != nil {
		return fail(400, "invalid_json", "Artifact contains unsupported JSON values.")
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return fail(400, "invalid_json", "Artifact contains unsupported JSON values.")
	}
	return validateJSON(value, 0, new(int))
}

func validateJSON(value any, depth int, nodes *int) error {
	*nodes++
	if depth > 32 || *nodes > 10_000 {
		return fail(400, "payload_too_complex", "JSON payload is too complex.")
	}
	switch typed := value.(type) {
	case map[string]any:
		if len(typed) > 1_000 {
			return fail(400, "payload_too_complex", "JSON object is too large.")
		}
		for key, child := range typed {
			if len(key) > 128 || key == "__proto__" || key == "constructor" || key == "prototype" {
				return fail(400, "invalid_key", "JSON key is not allowed.")
			}
			if err := validateJSON(child, depth+1, nodes); err != nil {
				return err
			}
		}
	case []any:
		if len(typed) > 1_000 {
			return fail(400, "payload_too_complex", "JSON array is too large.")
		}
		for _, child := range typed {
			if err := validateJSON(child, depth+1, nodes); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *Repository) get(guid string, all map[string]StoredArtifact) (StoredArtifact, error) {
	if err := validateGUID(guid, "GUID"); err != nil {
		return StoredArtifact{}, err
	}
	artifact, ok := all[strings.ToLower(guid)]
	if !ok {
		return StoredArtifact{}, fail(404, "not_found", "Artifact not found.")
	}
	return artifact, nil
}

func (r *Repository) Get(guid string) (StoredArtifact, error) {
	snapshot, err := r.snapshot(context.Background())
	if err != nil {
		return StoredArtifact{}, err
	}
	item, err := r.get(guid, snapshot.artifacts)
	if err != nil {
		return StoredArtifact{}, err
	}
	return cloneStored(item)
}

func (r *Repository) List(filters Filters) ([]StoredArtifact, error) {
	items, err := r.filteredArtifacts(filters)
	if err != nil {
		return nil, err
	}
	for index, item := range items {
		item, err = cloneStored(item)
		if err != nil {
			return nil, err
		}
		item.file, item.raw = "", nil
		items[index] = item
	}
	return items, nil
}

func (r *Repository) filteredArtifacts(filters Filters) ([]StoredArtifact, error) {
	if filters.Kind != "" && !kindValid(filters.Kind) {
		return nil, fail(400, "invalid_kind", "Artifact kind is invalid.")
	}
	if filters.Type != "" {
		if err := validateIdentifier(filters.Type, "Artifact type"); err != nil {
			return nil, err
		}
	}
	path := ""
	if filters.Path != "" {
		var err error
		path, err = safeFolder(filters.Path)
		if err != nil {
			return nil, err
		}
	}
	snapshot, err := r.snapshot(context.Background())
	if err != nil {
		return nil, err
	}
	result := make([]StoredArtifact, 0, len(snapshot.artifacts))
	for _, item := range snapshot.artifacts {
		if filters.Kind != "" && item.Artifact.Kind != filters.Kind || filters.Type != "" && item.Artifact.Type != filters.Type {
			continue
		}
		if path != "" && item.Path != path && !strings.HasPrefix(item.Path, path+"/") {
			continue
		}
		result = append(result, item)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Path != result[j].Path {
			return result[i].Path < result[j].Path
		}
		return result[i].Artifact.Title < result[j].Artifact.Title
	})
	return result, nil
}

func newGUID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	raw[6] = raw[6]&0x0f | 0x40
	raw[8] = raw[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x",
		raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16]), nil
}

func (r *Repository) Create(ctx context.Context, input CreateInput) (StoredArtifact, error) {
	unlock, err := r.lockWrite(ctx)
	if err != nil {
		return StoredArtifact{}, err
	}
	defer unlock()
	folder, err := safeFolder(input.Path)
	if err != nil {
		return StoredArtifact{}, err
	}
	guid, err := newGUID()
	if err != nil {
		return StoredArtifact{}, err
	}
	artifact := Artifact{
		GUID: guid, Kind: input.Kind, Type: input.Type, Title: input.Title, HID: input.HID,
		Base: strings.ToLower(input.Base), Fields: input.Fields, Content: input.Content, Source: strings.ToLower(input.Source),
		Target: strings.ToLower(input.Target), LinkType: input.LinkType, Subject: strings.ToLower(input.Subject), Parent: strings.ToLower(input.Parent),
		Body: input.Body,
	}
	if err := validateArtifact(artifact); err != nil {
		return StoredArtifact{}, err
	}
	snapshot, err := r.snapshot(ctx)
	if err != nil {
		return StoredArtifact{}, err
	}
	all := snapshot.artifacts
	if err := validateIntegrity(artifact, folder, all, snapshot.files); err != nil {
		return StoredArtifact{}, err
	}
	raw, err := marshalArtifact(artifact)
	if err != nil {
		return StoredArtifact{}, err
	}
	file := filepath.Join(r.root, filepath.FromSlash(folder), guid, "artifact.json")
	if err := r.ensureInside(file); err != nil {
		return StoredArtifact{}, err
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return StoredArtifact{}, err
	}
	if err := r.ensureInside(file); err != nil {
		return StoredArtifact{}, err
	}
	if err := writeExclusive(file, raw); err != nil {
		return StoredArtifact{}, err
	}
	if err := r.commit(ctx, file, commitMessage(artifact, "created")); err != nil {
		if committed, verifyErr := r.headMatches(relative(r.root, file), raw); verifyErr == nil && committed {
			return StoredArtifact{Artifact: artifact, ETag: checksum(raw), Path: folder}, nil
		}
		_ = os.RemoveAll(filepath.Dir(file))
		_, _ = r.git(context.Background(), "reset", "--quiet", "HEAD", "--", relative(r.root, file))
		return StoredArtifact{}, err
	}
	return StoredArtifact{Artifact: artifact, ETag: checksum(raw), Path: folder}, nil
}

func (r *Repository) ensureInside(path string) error {
	rel, err := filepath.Rel(r.root, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fail(400, "invalid_path", "Path escapes the repository.")
	}
	current := r.root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, statErr := os.Lstat(current)
		if errors.Is(statErr, fs.ErrNotExist) {
			continue
		}
		if statErr != nil {
			return statErr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fail(400, "invalid_path", "Managed paths cannot contain symbolic links.")
		}
	}
	return nil
}

func writeExclusive(path string, raw []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err = file.Write(raw); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(path)
		return err
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		_ = os.Remove(path)
		return err
	}
	return err
}

func writeAtomic(path string, raw []byte) error {
	temporaryFile, err := os.CreateTemp(filepath.Dir(path), ".origoa-*.tmp")
	if err != nil {
		return err
	}
	temporary := temporaryFile.Name()
	defer os.Remove(temporary)
	if err := temporaryFile.Chmod(0o600); err == nil {
		_, err = temporaryFile.Write(raw)
	}
	if err == nil {
		err = temporaryFile.Sync()
	}
	if closeErr := temporaryFile.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func (r *Repository) headMatches(name string, expected []byte) (bool, error) {
	raw, exists, err := r.headFile(context.Background(), name)
	return exists && bytes.Equal(raw, expected), err
}

func restoreArtifactFile(name string, raw []byte) {
	directory := filepath.Dir(name)
	_ = os.MkdirAll(directory, 0o700)
	_ = writeAtomic(name, raw)
	_ = syncDirectory(filepath.Dir(directory))
}

var syncDirectory = func(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func (r *Repository) commit(ctx context.Context, path, message string) error {
	rel := relative(r.root, path)
	if _, err := r.git(ctx, "add", "--all", "--", rel); err != nil {
		return err
	}
	_, err := r.git(ctx, "commit", "--quiet", "-m", message, "--", rel)
	return err
}

func relative(root, path string) string {
	rel, _ := filepath.Rel(root, path)
	return filepath.ToSlash(rel)
}

func commitMessage(artifact Artifact, operation string) string {
	name := strings.ToUpper(string(artifact.Kind[:1])) + string(artifact.Kind[1:])
	return fmt.Sprintf("%s %s %s", name, artifact.GUID, operation)
}

var mutableProperties = map[string]bool{
	"type": true, "title": true, "hid": true, "base": true, "fields": true, "content": true,
	"source": true, "target": true, "linkType": true, "subject": true, "parent": true,
	"body": true,
}

func (r *Repository) Update(ctx context.Context, guid string, patch map[string]any, expectedETag string) (StoredArtifact, error) {
	unlock, err := r.lockWrite(ctx)
	if err != nil {
		return StoredArtifact{}, err
	}
	defer unlock()
	return r.updateLocked(ctx, guid, patch, expectedETag, "updated", false)
}

func (r *Repository) updateLocked(ctx context.Context, guid string, patch map[string]any, expectedETag, operation string, allowWorkflow bool) (StoredArtifact, error) {
	for key := range patch {
		if !mutableProperties[key] && !(allowWorkflow && key == "workflows") {
			return StoredArtifact{}, fail(400, "immutable_or_unknown_property", "Property '"+key+"' cannot be changed.")
		}
	}
	if err := validateJSON(patch, 0, new(int)); err != nil {
		return StoredArtifact{}, err
	}
	snapshot, err := r.snapshot(ctx)
	if err != nil {
		return StoredArtifact{}, err
	}
	all := maps.Clone(snapshot.artifacts)
	current, err := r.get(guid, all)
	if err != nil {
		return StoredArtifact{}, err
	}
	if current.ETag != expectedETag {
		return StoredArtifact{}, fail(412, "version_conflict", "Artifact has changed; reload before saving.")
	}
	artifact, err := clone(current.Artifact)
	if err != nil {
		return StoredArtifact{}, err
	}
	if err := applyPatch(&artifact, patch); err != nil {
		return StoredArtifact{}, err
	}
	if err := validateArtifact(artifact); err != nil {
		return StoredArtifact{}, err
	}
	delete(all, strings.ToLower(guid))
	if err := validateIntegrity(artifact, current.Path, all, snapshot.files); err != nil {
		return StoredArtifact{}, err
	}
	raw, err := marshalArtifact(artifact)
	if err != nil {
		return StoredArtifact{}, err
	}
	if bytes.Equal(raw, current.raw) {
		return StoredArtifact{Artifact: artifact, ETag: current.ETag, Path: current.Path}, nil
	}
	if err := r.ensureInside(current.file); err != nil {
		return StoredArtifact{}, err
	}
	if err := os.MkdirAll(filepath.Dir(current.file), 0o700); err != nil {
		return StoredArtifact{}, err
	}
	if err := r.ensureInside(current.file); err != nil {
		return StoredArtifact{}, err
	}
	if err := writeAtomic(current.file, raw); err != nil {
		restoreArtifactFile(current.file, current.raw)
		return StoredArtifact{}, err
	}
	if err := r.commit(ctx, current.file, commitMessage(artifact, operation)); err != nil {
		if committed, verifyErr := r.headMatches(relative(r.root, current.file), raw); verifyErr == nil && committed {
			return StoredArtifact{Artifact: artifact, ETag: checksum(raw), Path: current.Path}, nil
		}
		restoreArtifactFile(current.file, current.raw)
		_, _ = r.git(context.Background(), "reset", "--quiet", "HEAD", "--", relative(r.root, current.file))
		return StoredArtifact{}, err
	}
	return StoredArtifact{Artifact: artifact, ETag: checksum(raw), Path: current.Path}, nil
}

func applyPatch(artifact *Artifact, patch map[string]any) error {
	raw, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return err
	}
	for key, value := range values {
		remove := bytes.Equal(value, []byte("null"))
		switch key {
		case "type":
			if remove || json.Unmarshal(value, &artifact.Type) != nil {
				return fail(400, "invalid_value", "Type is invalid.")
			}
		case "title":
			if remove || json.Unmarshal(value, &artifact.Title) != nil {
				return fail(400, "invalid_value", "Title is invalid.")
			}
		case "hid":
			if remove {
				artifact.HID = ""
			} else if json.Unmarshal(value, &artifact.HID) != nil {
				return fail(400, "invalid_value", "HID is invalid.")
			}
		case "base":
			if remove {
				artifact.Base = ""
			} else if json.Unmarshal(value, &artifact.Base) != nil {
				return fail(400, "invalid_value", "Base is invalid.")
			}
			artifact.Base = strings.ToLower(artifact.Base)
		case "fields":
			if remove {
				artifact.Fields = nil
			} else {
				var fields map[string]any
				if decodeNumber(value, &fields) != nil {
					return fail(400, "invalid_value", "Fields are invalid.")
				}
				artifact.Fields = fields
			}
		case "content":
			if remove {
				artifact.Content = nil
			} else if decodeNumber(value, &artifact.Content) != nil {
				return fail(400, "invalid_value", "Content is invalid.")
			}
		case "source":
			if remove {
				artifact.Source = ""
			} else if json.Unmarshal(value, &artifact.Source) != nil {
				return fail(400, "invalid_value", "Source is invalid.")
			}
			artifact.Source = strings.ToLower(artifact.Source)
		case "target":
			if remove {
				artifact.Target = ""
			} else if json.Unmarshal(value, &artifact.Target) != nil {
				return fail(400, "invalid_value", "Target is invalid.")
			}
			artifact.Target = strings.ToLower(artifact.Target)
		case "linkType":
			if remove {
				artifact.LinkType = ""
			} else if json.Unmarshal(value, &artifact.LinkType) != nil {
				return fail(400, "invalid_value", "Link type is invalid.")
			}
		case "subject":
			if remove {
				artifact.Subject = ""
			} else if json.Unmarshal(value, &artifact.Subject) != nil {
				return fail(400, "invalid_value", "Subject is invalid.")
			}
			artifact.Subject = strings.ToLower(artifact.Subject)
		case "parent":
			if remove {
				artifact.Parent = ""
			} else if json.Unmarshal(value, &artifact.Parent) != nil {
				return fail(400, "invalid_value", "Parent is invalid.")
			}
			artifact.Parent = strings.ToLower(artifact.Parent)
		case "body":
			if remove {
				artifact.Body = ""
			} else if json.Unmarshal(value, &artifact.Body) != nil {
				return fail(400, "invalid_value", "Body is invalid.")
			}
		case "workflows":
			if remove {
				artifact.Workflows = nil
			} else {
				var workflows map[string]string
				if json.Unmarshal(value, &workflows) != nil {
					return fail(400, "invalid_value", "Workflows are invalid.")
				}
				artifact.Workflows = workflows
			}
		}
	}
	return nil
}

func decodeNumber(raw []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("multiple JSON values")
	}
	return nil
}

func (r *Repository) Delete(ctx context.Context, guid, expectedETag string) error {
	unlock, err := r.lockWrite(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	snapshot, err := r.snapshot(ctx)
	if err != nil {
		return err
	}
	all := snapshot.artifacts
	current, err := r.get(guid, all)
	if err != nil {
		return err
	}
	if current.ETag != expectedETag {
		return fail(412, "version_conflict", "Artifact has changed; reload before deleting.")
	}
	for _, item := range all {
		artifact := item.Artifact
		if artifact.GUID != current.Artifact.GUID && slices.ContainsFunc(
			[]string{artifact.Base, artifact.Source, artifact.Target, artifact.Subject, artifact.Parent},
			func(reference string) bool { return strings.EqualFold(reference, current.Artifact.GUID) },
		) {
			return fail(409, "artifact_referenced", "Artifact is still referenced.")
		}
		if artifact.GUID == current.Artifact.GUID {
			continue
		}
		schema, err := effectiveSchema(snapshot.files, artifact.Type, item.Path)
		if err != nil {
			return err
		}
		fields, err := resolvedFields(artifact, all)
		if err != nil {
			return err
		}
		for _, reference := range fieldReferences(fields, object(schema["fields"])) {
			if strings.EqualFold(reference, current.Artifact.GUID) {
				return fail(409, "artifact_referenced", "Artifact is still referenced.")
			}
		}
	}
	directory := filepath.Dir(current.file)
	if err := r.ensureInside(current.file); err != nil {
		return err
	}
	if err := os.RemoveAll(directory); err != nil {
		return err
	}
	if err := syncDirectory(filepath.Dir(directory)); err != nil {
		restoreArtifactFile(current.file, current.raw)
		return err
	}
	if err := r.commit(ctx, directory, commitMessage(current.Artifact, "deleted")); err != nil {
		if _, exists, verifyErr := r.headFile(context.Background(), relative(r.root, current.file)); verifyErr == nil && !exists {
			return nil
		}
		restoreArtifactFile(current.file, current.raw)
		_, _ = r.git(context.Background(), "reset", "--quiet", "HEAD", "--", relative(r.root, directory))
		return err
	}
	return nil
}

func validateIntegrity(artifact Artifact, folder string, all map[string]StoredArtifact, files map[string][]byte) error {
	if artifact.HID != "" {
		for _, item := range all {
			if strings.EqualFold(item.Artifact.HID, artifact.HID) {
				return fail(409, "duplicate_hid", "HID already exists.")
			}
		}
	}
	if artifact.Base != "" {
		base, ok := all[strings.ToLower(artifact.Base)]
		if !ok || artifact.Kind != Entry || base.Artifact.Kind != Entry {
			return fail(400, "invalid_overlay", "Overlay base must reference an entry.")
		}
		seen := map[string]bool{strings.ToLower(artifact.GUID): true}
		cursor := base.Artifact
		for depth := 0; cursor.Base != ""; depth++ {
			key := strings.ToLower(cursor.Base)
			if depth > 31 || seen[key] {
				return fail(409, "overlay_cycle", "Overlay cycle detected.")
			}
			seen[key] = true
			next, ok := all[key]
			if !ok {
				return fail(400, "broken_reference", "Overlay base does not exist.")
			}
			cursor = next.Artifact
		}
	}
	for label, guid := range map[string]string{
		"source": artifact.Source, "target": artifact.Target, "subject": artifact.Subject, "parent": artifact.Parent,
	} {
		if guid != "" {
			if _, ok := all[strings.ToLower(guid)]; !ok {
				return fail(400, "broken_reference", label+" does not exist.")
			}
		}
	}
	schema, err := effectiveSchema(files, artifact.Type, folder)
	if err != nil {
		return err
	}
	definitions := object(schema["fields"])
	fields, err := resolvedFields(artifact, all)
	if err != nil {
		return err
	}
	if err := validateFields(fields, definitions); err != nil {
		return err
	}
	for _, reference := range fieldReferences(fields, definitions) {
		if strings.EqualFold(reference, artifact.GUID) {
			continue
		}
		if _, ok := all[strings.ToLower(reference)]; !ok {
			return fail(400, "broken_reference", "Artifact field reference does not exist.")
		}
	}
	assigned := configuredWorkflows(schema)
	for id := range artifact.Workflows {
		if !assigned[id] {
			return fail(400, "workflow_not_assigned", "Workflow '"+id+"' is not assigned by the schema.")
		}
	}
	for id := range assigned {
		definition, err := readWorkflow(files, id, folder)
		if err != nil {
			var repoError *Error
			if errors.As(err, &repoError) && repoError.Code == "workflow_not_found" {
				return fail(500, "repository_corrupt", "Schema assigns missing workflow '"+id+"'.")
			}
			return err
		}
		state := artifact.Workflows[id]
		if state == "" {
			state = definition.Initial
		}
		if !slices.Contains(definition.States, state) {
			return fail(400, "invalid_workflow_state", "Workflow state is invalid.")
		}
	}
	return nil
}

func resolvedFields(artifact Artifact, all map[string]StoredArtifact) (map[string]any, error) {
	chain := []Artifact{artifact}
	seen := map[string]bool{strings.ToLower(artifact.GUID): true}
	for cursor := artifact; cursor.Base != ""; {
		key := strings.ToLower(cursor.Base)
		if seen[key] || len(chain) > 32 {
			return nil, fail(409, "overlay_cycle", "Overlay cycle detected.")
		}
		seen[key] = true
		base, ok := all[key]
		if !ok {
			return nil, fail(409, "broken_reference", "Overlay base does not exist.")
		}
		chain = append([]Artifact{base.Artifact}, chain...)
		cursor = base.Artifact
	}
	result := map[string]any{}
	for _, item := range chain {
		for name, value := range item.Fields {
			result[name], _ = clone(value)
		}
	}
	return result, nil
}

func configuredWorkflows(schema map[string]any) map[string]bool {
	result := map[string]bool{}
	if configured, ok := schema["workflows"].([]any); ok {
		for _, raw := range configured {
			if id, ok := raw.(string); ok {
				result[id] = true
			}
		}
	}
	return result
}

func fieldReferences(fields, definitions map[string]any) []string {
	result := []string{}
	for name, rawDefinition := range definitions {
		definition := object(rawDefinition)
		fieldType, _ := definition["type"].(string)
		if !slices.Contains([]string{"artifact-reference", "reference", "multi-artifact-reference"}, fieldType) {
			continue
		}
		value := fields[name]
		if items, ok := value.([]any); ok {
			for _, item := range items {
				if reference, ok := item.(string); ok {
					result = append(result, reference)
				}
			}
		} else if reference, ok := value.(string); ok {
			result = append(result, reference)
		}
	}
	return result
}

func object(value any) map[string]any {
	if result, ok := value.(map[string]any); ok {
		return result
	}
	return map[string]any{}
}

func merge(base, local map[string]any) map[string]any {
	result, _ := clone(base)
	for key, value := range local {
		if localObject, ok := value.(map[string]any); ok {
			if baseObject, ok := result[key].(map[string]any); ok {
				result[key] = merge(baseObject, localObject)
				continue
			}
		}
		result[key], _ = clone(value)
	}
	return result
}

func (r *Repository) EffectiveSchema(artifactType, folder string) (map[string]any, error) {
	snapshot, err := r.snapshot(context.Background())
	if err != nil {
		return nil, err
	}
	return effectiveSchema(snapshot.files, artifactType, folder)
}

func effectiveSchema(files map[string][]byte, artifactType, folder string) (map[string]any, error) {
	if err := validateIdentifier(artifactType, "Artifact type"); err != nil {
		return nil, err
	}
	if folder != "" {
		var err error
		folder, err = safeFolder(folder)
		if err != nil {
			return nil, err
		}
	}
	parts := strings.FieldsFunc(folder, func(r rune) bool { return r == '/' })
	effective := map[string]any{"id": artifactType, "fields": map[string]any{}}
	for index := 0; index <= len(parts); index++ {
		name := pathpkg.Join(pathpkg.Join(parts[:index]...), ".origoa", "schemas", artifactType+".json")
		raw, exists := files[name]
		if !exists {
			continue
		}
		var local map[string]any
		if decodeNumber(raw, &local) != nil {
			return nil, fail(500, "repository_corrupt", "Invalid schema "+name+".")
		}
		if local["inheritance"] == "off" {
			effective = map[string]any{"id": artifactType, "fields": map[string]any{}}
		}
		effective = merge(effective, local)
	}
	if err := validateEffectiveSchema(effective); err != nil {
		return nil, err
	}
	return effective, nil
}

var supportedFieldTypes = map[string]bool{
	"text": true, "hid": true, "single-line": true, "multi-line": true, "rich-text": true,
	"date": true, "time": true, "date-time": true, "boolean": true, "number": true,
	"float": true, "currency": true, "integer": true, "enumeration": true,
	"artifact-reference": true, "reference": true, "multi-artifact-reference": true,
	"hyperlink": true, "json": true, "object": true, "attachment": true, "workflow": true,
}

func validateEffectiveSchema(schema map[string]any) error {
	fields, ok := schema["fields"].(map[string]any)
	if !ok {
		return fail(500, "repository_corrupt", "Schema fields must be an object.")
	}
	for name, raw := range fields {
		definition, ok := raw.(map[string]any)
		if !ok {
			return fail(500, "repository_corrupt", "Schema field '"+name+"' must be an object.")
		}
		fieldType := "text"
		if rawType, exists := definition["type"]; exists {
			var ok bool
			fieldType, ok = rawType.(string)
			if !ok || fieldType == "" {
				return fail(500, "repository_corrupt", "Schema field '"+name+"' has an invalid type.")
			}
		}
		if !supportedFieldTypes[fieldType] {
			return fail(500, "repository_corrupt", "Schema field '"+name+"' has an unsupported type.")
		}
		if required, exists := definition["required"]; exists {
			if _, ok := required.(bool); !ok {
				return fail(500, "repository_corrupt", "Schema field '"+name+"' has invalid required metadata.")
			}
		}
		if maximum, exists := definition["maxLength"]; exists {
			value, ok := numberValue(maximum)
			if !ok || value < 0 || value > maxManagedFile || math.Trunc(value) != value {
				return fail(500, "repository_corrupt", "Schema field '"+name+"' has invalid maxLength metadata.")
			}
		}
		if displayName, exists := definition["displayName"]; exists {
			if _, ok := displayName.(string); !ok {
				return fail(500, "repository_corrupt", "Schema field '"+name+"' has invalid display metadata.")
			}
		}
		if multiplicity, exists := definition["multiplicity"]; exists {
			value, ok := multiplicity.(string)
			if !ok || value != "one" && value != "many" {
				return fail(500, "repository_corrupt", "Schema field '"+name+"' has invalid multiplicity.")
			}
		}
		if values, exists := definition["values"]; exists {
			items, ok := values.([]any)
			if !ok {
				return fail(500, "repository_corrupt", "Schema field '"+name+"' has invalid enumeration values.")
			}
			seen := map[string]bool{}
			for _, rawValue := range items {
				value, ok := rawValue.(string)
				if !ok || seen[value] {
					return fail(500, "repository_corrupt", "Schema field '"+name+"' has invalid enumeration values.")
				}
				seen[value] = true
			}
			if fieldType == "enumeration" && len(items) == 0 {
				return fail(500, "repository_corrupt", "Enumeration field '"+name+"' must define values.")
			}
		} else if fieldType == "enumeration" {
			return fail(500, "repository_corrupt", "Enumeration field '"+name+"' must define values.")
		}
	}
	if workflows, exists := schema["workflows"]; exists {
		items, ok := workflows.([]any)
		if !ok {
			return fail(500, "repository_corrupt", "Schema workflows must be an array.")
		}
		for _, raw := range items {
			id, ok := raw.(string)
			if !ok || !identifierPattern.MatchString(id) {
				return fail(500, "repository_corrupt", "Schema contains an invalid workflow assignment.")
			}
		}
	}
	return nil
}

func validateFields(fields, definitions map[string]any) error {
	for name := range fields {
		if len(definitions) > 0 {
			if _, ok := definitions[name]; !ok {
				return fail(400, "unknown_field", "Field '"+name+"' is not defined by the schema.")
			}
		}
	}
	for name, rawDefinition := range definitions {
		definition := object(rawDefinition)
		value, present := fields[name]
		if required, _ := definition["required"].(bool); required && (!present || value == nil || value == "") {
			return fail(400, "required_field", "Field '"+name+"' is required.")
		}
		if !present || value == nil {
			continue
		}
		if definition["multiplicity"] == "many" {
			items, ok := value.([]any)
			if !ok {
				return fail(400, "invalid_field", "Field '"+name+"' must be an array.")
			}
			for _, item := range items {
				if err := validateFieldValue(name, item, definition); err != nil {
					return err
				}
			}
		} else if err := validateFieldValue(name, value, definition); err != nil {
			return err
		}
	}
	return nil
}

func validateFieldValue(name string, value any, definition map[string]any) error {
	fieldType, _ := definition["type"].(string)
	if fieldType == "" {
		fieldType = "text"
	}
	textTypes := []string{"hid", "text", "single-line", "multi-line", "rich-text", "date", "time", "date-time"}
	if slices.Contains(textTypes, fieldType) {
		text, ok := value.(string)
		if !ok {
			return fail(400, "invalid_field", "Field '"+name+"' must be text.")
		}
		if maximum, ok := numberValue(definition["maxLength"]); ok && len(text) > int(maximum) {
			return fail(400, "invalid_field", "Field '"+name+"' is too long.")
		}
	}
	if fieldType == "boolean" {
		if _, ok := value.(bool); !ok {
			return fail(400, "invalid_field", "Field '"+name+"' must be boolean.")
		}
	}
	if slices.Contains([]string{"number", "float", "currency"}, fieldType) {
		if _, ok := numberValue(value); !ok {
			return fail(400, "invalid_field", "Field '"+name+"' must be a number.")
		}
	}
	if fieldType == "integer" {
		if !integerValue(value) {
			return fail(400, "invalid_field", "Field '"+name+"' must be an integer.")
		}
	}
	if fieldType == "enumeration" {
		values, _ := definition["values"].([]any)
		selected, ok := value.(string)
		if !ok || !slices.ContainsFunc(values, func(allowed any) bool { return allowed == selected }) {
			return fail(400, "invalid_field", "Field '"+name+"' is not an allowed value.")
		}
	}
	if fieldType == "artifact-reference" || fieldType == "reference" || fieldType == "multi-artifact-reference" {
		text, ok := value.(string)
		if !ok || validateGUID(text, "Field '"+name+"'") != nil {
			return fail(400, "invalid_field", "Field '"+name+"' must be a GUID.")
		}
	}
	if fieldType == "hyperlink" {
		text, ok := value.(string)
		parsed, err := url.Parse(text)
		if !ok || err != nil || parsed.Host == "" || parsed.Scheme != "http" && parsed.Scheme != "https" {
			return fail(400, "invalid_field", "Field '"+name+"' must be an HTTP(S) URL.")
		}
	}
	if slices.Contains([]string{"json", "object", "attachment", "workflow"}, fieldType) {
		if _, ok := value.(map[string]any); !ok {
			return fail(400, "invalid_field", "Field '"+name+"' must be an object.")
		}
	}
	return nil
}

func numberValue(value any) (float64, bool) {
	switch number := value.(type) {
	case json.Number:
		parsed, err := number.Float64()
		return parsed, err == nil && !math.IsInf(parsed, 0) && !math.IsNaN(parsed)
	case float64:
		return number, !math.IsInf(number, 0) && !math.IsNaN(number)
	case float32:
		return float64(number), !math.IsInf(float64(number), 0) && !math.IsNaN(float64(number))
	case int:
		return float64(number), true
	case int8:
		return float64(number), true
	case int16:
		return float64(number), true
	case int32:
		return float64(number), true
	case int64:
		return float64(number), true
	case uint:
		return float64(number), true
	case uint8:
		return float64(number), true
	case uint16:
		return float64(number), true
	case uint32:
		return float64(number), true
	case uint64:
		return float64(number), true
	default:
		return 0, false
	}
}

func integerValue(value any) bool {
	switch number := value.(type) {
	case json.Number:
		var exact big.Rat
		_, ok := exact.SetString(number.String())
		return ok && exact.IsInt()
	case float64:
		return !math.IsInf(number, 0) && !math.IsNaN(number) && math.Trunc(number) == number
	case float32:
		value := float64(number)
		return !math.IsInf(value, 0) && !math.IsNaN(value) && math.Trunc(value) == value
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return true
	default:
		return false
	}
}

func (r *Repository) ResolveOverlay(guid string) (Overlay, error) {
	snapshot, err := r.snapshot(context.Background())
	if err != nil {
		return Overlay{}, err
	}
	all := snapshot.artifacts
	top, err := r.get(guid, all)
	if err != nil {
		return Overlay{}, err
	}
	if top.Artifact.Kind != Entry {
		return Overlay{}, fail(400, "not_an_entry", "Only entries can be overlays.")
	}
	chain := []Artifact{}
	seen := map[string]bool{}
	cursor := top.Artifact
	for {
		key := strings.ToLower(cursor.GUID)
		if seen[key] || len(chain) > 32 {
			return Overlay{}, fail(409, "overlay_cycle", "Overlay cycle detected.")
		}
		seen[key] = true
		chain = append([]Artifact{cursor}, chain...)
		if cursor.Base == "" {
			break
		}
		next, ok := all[strings.ToLower(cursor.Base)]
		if !ok {
			return Overlay{}, fail(409, "broken_reference", "Overlay base does not exist.")
		}
		cursor = next.Artifact
	}
	resolved, _ := clone(chain[0])
	ids := make([]string, 0, len(chain))
	for index, overlay := range chain {
		ids = append(ids, overlay.GUID)
		if index == 0 {
			continue
		}
		fields := map[string]any{}
		for name, value := range resolved.Fields {
			fields[name], _ = clone(value)
		}
		for name, value := range overlay.Fields {
			fields[name], _ = clone(value)
		}
		resolved = overlay
		resolved.Fields = fields
	}
	return Overlay{Artifact: resolved, Chain: ids}, nil
}

func (r *Repository) Links(guid string) (Links, error) {
	if err := validateGUID(guid, "GUID"); err != nil {
		return Links{}, err
	}
	if _, err := r.Get(guid); err != nil {
		return Links{}, err
	}
	links, err := r.filteredArtifacts(Filters{Kind: Link})
	if err != nil {
		return Links{}, err
	}
	result := Links{Incoming: []StoredArtifact{}, Outgoing: []StoredArtifact{}}
	for _, item := range links {
		if !strings.EqualFold(item.Artifact.Target, guid) && !strings.EqualFold(item.Artifact.Source, guid) {
			continue
		}
		item, err = cloneStored(item)
		if err != nil {
			return Links{}, err
		}
		item.file, item.raw = "", nil
		if strings.EqualFold(item.Artifact.Target, guid) {
			result.Incoming = append(result.Incoming, item)
		}
		if strings.EqualFold(item.Artifact.Source, guid) {
			result.Outgoing = append(result.Outgoing, item)
		}
	}
	return result, nil
}

func (r *Repository) Search(input SearchInput) ([]StoredArtifact, error) {
	query := strings.ToLower(strings.TrimSpace(input.Query))
	if len(query) > 200 {
		return nil, fail(400, "invalid_query", "Search query is too long.")
	}
	if input.Limit == 0 {
		input.Limit = 50
	}
	if input.Limit < 1 || input.Limit > 200 {
		return nil, fail(400, "invalid_limit", "Limit must be 1-200.")
	}
	items, err := r.filteredArtifacts(Filters{Kind: input.Kind, Type: input.Type})
	if err != nil {
		return nil, err
	}
	result := make([]StoredArtifact, 0, min(input.Limit, len(items)))
	for _, item := range items {
		raw, _ := json.Marshal(item.Artifact)
		if query == "" || strings.Contains(strings.ToLower(string(raw)), query) {
			item, err = cloneStored(item)
			if err != nil {
				return nil, err
			}
			item.file, item.raw = "", nil
			result = append(result, item)
			if len(result) == input.Limit {
				break
			}
		}
	}
	return result, nil
}

func (r *Repository) Tree() (map[string]any, error) {
	root := map[string]any{"name": "", "folders": map[string]any{}, "artifacts": []any{}}
	items, err := r.filteredArtifacts(Filters{})
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		cursor := root
		for _, segment := range strings.Split(item.Path, "/") {
			if segment == "" {
				continue
			}
			folders := cursor["folders"].(map[string]any)
			if folders[segment] == nil {
				folders[segment] = map[string]any{"name": segment, "folders": map[string]any{}, "artifacts": []any{}}
			}
			cursor = folders[segment].(map[string]any)
		}
		cursor["artifacts"] = append(cursor["artifacts"].([]any), map[string]any{
			"guid": item.Artifact.GUID, "kind": item.Artifact.Kind, "type": item.Artifact.Type, "title": item.Artifact.Title,
		})
	}
	return root, nil
}

func readWorkflow(files map[string][]byte, id, folder string) (workflow, error) {
	if err := validateIdentifier(id, "Workflow"); err != nil {
		return workflow{}, err
	}
	parts := strings.Split(folder, "/")
	if folder == "" {
		parts = nil
	}
	for index := len(parts); index >= 0; index-- {
		name := pathpkg.Join(pathpkg.Join(parts[:index]...), ".origoa", "workflows", id+".json")
		raw, exists := files[name]
		if !exists {
			continue
		}
		var value workflow
		if decodeStrict(raw, &value) != nil || value.ID != id || !identifierPattern.MatchString(value.Initial) || !slices.Contains(value.States, value.Initial) {
			return workflow{}, fail(500, "repository_corrupt", "Invalid workflow "+name+".")
		}
		states := map[string]bool{}
		for _, state := range value.States {
			if !identifierPattern.MatchString(state) || states[state] {
				return workflow{}, fail(500, "repository_corrupt", "Workflow contains an invalid state.")
			}
			states[state] = true
		}
		transitions := map[string]bool{}
		for _, transition := range value.Transitions {
			if !identifierPattern.MatchString(transition.ID) || transitions[transition.ID] || !states[transition.From] || !states[transition.To] {
				return workflow{}, fail(500, "repository_corrupt", "Workflow contains an invalid transition.")
			}
			transitions[transition.ID] = true
		}
		return value, nil
	}
	return workflow{}, fail(404, "workflow_not_found", "Workflow not found.")
}

func (r *Repository) Workflows(guid string) ([]WorkflowInfo, error) {
	snapshot, err := r.snapshot(context.Background())
	if err != nil {
		return nil, err
	}
	item, err := r.get(guid, snapshot.artifacts)
	if err != nil {
		return nil, err
	}
	schema, err := effectiveSchema(snapshot.files, item.Artifact.Type, item.Path)
	if err != nil {
		return nil, err
	}
	ids := configuredWorkflows(schema)
	ordered := make([]string, 0, len(ids))
	for id := range ids {
		ordered = append(ordered, id)
	}
	sort.Strings(ordered)
	result := make([]WorkflowInfo, 0, len(ordered))
	for _, id := range ordered {
		definition, err := readWorkflow(snapshot.files, id, item.Path)
		if err != nil {
			return nil, err
		}
		state := item.Artifact.Workflows[id]
		if state == "" {
			state = definition.Initial
		}
		if !slices.Contains(definition.States, state) {
			return nil, fail(500, "repository_corrupt", "Artifact contains an invalid workflow state.")
		}
		info := WorkflowInfo{ID: id, State: state, Transitions: []WorkflowTransition{}}
		for _, transition := range definition.Transitions {
			if transition.From == state {
				info.Transitions = append(info.Transitions, transition)
			}
		}
		result = append(result, info)
	}
	return result, nil
}

func (r *Repository) Transition(ctx context.Context, guid, workflowID, transitionID, expectedETag string) (StoredArtifact, error) {
	unlock, err := r.lockWrite(ctx)
	if err != nil {
		return StoredArtifact{}, err
	}
	defer unlock()
	if err := validateIdentifier(workflowID, "Workflow"); err != nil {
		return StoredArtifact{}, err
	}
	if err := validateIdentifier(transitionID, "Transition"); err != nil {
		return StoredArtifact{}, err
	}
	snapshot, err := r.snapshot(ctx)
	if err != nil {
		return StoredArtifact{}, err
	}
	all := snapshot.artifacts
	current, err := r.get(guid, all)
	if err != nil {
		return StoredArtifact{}, err
	}
	if current.ETag != expectedETag {
		return StoredArtifact{}, fail(412, "version_conflict", "Artifact has changed; reload before saving.")
	}
	schema, err := effectiveSchema(snapshot.files, current.Artifact.Type, current.Path)
	if err != nil {
		return StoredArtifact{}, err
	}
	if !configuredWorkflows(schema)[workflowID] {
		return StoredArtifact{}, fail(409, "workflow_not_assigned", "Workflow is not assigned by the schema.")
	}
	definition, err := readWorkflow(snapshot.files, workflowID, current.Path)
	if err != nil {
		return StoredArtifact{}, err
	}
	state := current.Artifact.Workflows[workflowID]
	if state == "" {
		state = definition.Initial
	}
	var selected *WorkflowTransition
	for index := range definition.Transitions {
		if definition.Transitions[index].ID == transitionID && definition.Transitions[index].From == state {
			selected = &definition.Transitions[index]
			break
		}
	}
	if selected == nil {
		return StoredArtifact{}, fail(409, "transition_not_allowed", "Workflow transition is not allowed.")
	}
	states := make(map[string]string, len(current.Artifact.Workflows)+1)
	for id, value := range current.Artifact.Workflows {
		states[id] = value
	}
	states[workflowID] = selected.To
	operation := fmt.Sprintf("workflow %s transitioned %s to %s", workflowID, state, selected.To)
	return r.updateLocked(ctx, guid, map[string]any{"workflows": states}, expectedETag, operation, true)
}

func (r *Repository) History(ctx context.Context, guid string, limit int) ([]HistoryItem, error) {
	if limit == 0 {
		limit = 50
	}
	if limit < 1 || limit > 200 {
		return nil, fail(400, "invalid_limit", "Limit must be 1-200.")
	}
	item, err := r.Get(guid)
	if err != nil {
		return nil, err
	}
	output, err := r.git(ctx, "log", "--max-count="+strconv.Itoa(limit), "--format=%H%x1f%aI%x1f%an%x1f%s", "--", relative(r.root, item.file))
	if err != nil {
		return nil, err
	}
	result := []HistoryItem{}
	for _, line := range strings.Split(output, "\n") {
		parts := strings.Split(line, "\x1f")
		if len(parts) == 4 {
			result = append(result, HistoryItem{Commit: parts[0], Date: parts[1], Author: parts[2], Subject: parts[3]})
		}
	}
	return result, nil
}

func (r *Repository) Revision(ctx context.Context) (string, error) {
	return r.git(ctx, "rev-parse", "HEAD")
}

package repository

import (
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
	"time"
)

var (
	uuidPattern       = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	identifierPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9._-]{0,63}$`)
	hidPattern        = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	pathPartPattern   = regexp.MustCompile(`^[\pL\pN][\pL\pN._ -]{0,63}$`)
)

const maxContentLength = 500_000

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
	root string
	mu   sync.Mutex
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
	if err := r.initialize(ctx); err != nil {
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
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "git", append([]string{"-C", r.root}, args...)...)
	command.Env = append(os.Environ(), "LC_ALL=C")
	output, err := command.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

func (r *Repository) headFile(ctx context.Context, name string) ([]byte, bool, error) {
	object := "HEAD:" + name
	if _, err := r.gitBytes(ctx, "cat-file", "-e", object); err != nil {
		return nil, false, nil
	}
	raw, err := r.gitBytes(ctx, "show", object)
	return raw, err == nil, err
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

func clone[T any](value T) (T, error) {
	var result T
	raw, err := json.Marshal(value)
	if err == nil {
		err = json.Unmarshal(raw, &result)
	}
	return result, err
}

func (r *Repository) scan() (map[string]StoredArtifact, error) {
	found := make(map[string]StoredArtifact)
	ctx := context.Background()
	listing, err := r.gitBytes(ctx, "ls-tree", "-r", "-z", "--name-only", "HEAD")
	if err != nil {
		return nil, err
	}
	for _, entry := range bytes.Split(listing, []byte{0}) {
		name := string(entry)
		if name == "" || pathpkg.Base(name) != "artifact.json" || !uuidPattern.MatchString(pathpkg.Base(pathpkg.Dir(name))) {
			continue
		}
		raw, err := r.gitBytes(ctx, "show", "HEAD:"+name)
		if err != nil {
			return nil, err
		}
		var artifact Artifact
		if err := decodeStrict(raw, &artifact); err != nil {
			return nil, fail(500, "repository_corrupt", "Invalid artifact in "+name+": "+err.Error())
		}
		if err := validateArtifact(artifact); err != nil {
			return nil, fail(500, "repository_corrupt", "Invalid artifact in "+name+": "+err.Error())
		}
		if !strings.EqualFold(pathpkg.Base(pathpkg.Dir(name)), artifact.GUID) {
			return nil, fail(500, "repository_corrupt", "Artifact GUID does not match its directory.")
		}
		key := strings.ToLower(artifact.GUID)
		if _, duplicate := found[key]; duplicate {
			return nil, fail(500, "repository_corrupt", "Duplicate artifact GUID "+artifact.GUID+".")
		}
		relativePath := pathpkg.Dir(pathpkg.Dir(name))
		if relativePath == "." {
			relativePath = ""
		}
		found[key] = StoredArtifact{
			Artifact: artifact,
			ETag:     checksum(raw),
			Path:     relativePath,
			file:     filepath.Join(r.root, filepath.FromSlash(name)),
			raw:      raw,
		}
	}
	return found, nil
}

func decodeStrict(raw []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
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
	all, err := r.scan()
	if err != nil {
		return StoredArtifact{}, err
	}
	return r.get(guid, all)
}

func (r *Repository) List(filters Filters) ([]StoredArtifact, error) {
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
	all, err := r.scan()
	if err != nil {
		return nil, err
	}
	result := make([]StoredArtifact, 0, len(all))
	for _, item := range all {
		if filters.Kind != "" && item.Artifact.Kind != filters.Kind || filters.Type != "" && item.Artifact.Type != filters.Type {
			continue
		}
		if path != "" && item.Path != path && !strings.HasPrefix(item.Path, path+"/") {
			continue
		}
		item.file, item.raw = "", nil
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
	r.mu.Lock()
	defer r.mu.Unlock()
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
	all, err := r.scan()
	if err != nil {
		return StoredArtifact{}, err
	}
	if err := r.validateIntegrity(artifact, folder, all); err != nil {
		return StoredArtifact{}, err
	}
	raw, err := marshal(artifact)
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
		_ = os.RemoveAll(filepath.Dir(file))
		_, _ = r.git(ctx, "reset", "--quiet", "HEAD", "--", relative(r.root, file))
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
	temporary := path + "." + strconv.FormatInt(time.Now().UnixNano(), 36) + ".tmp"
	if err := writeExclusive(temporary, raw); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func syncDirectory(path string) error {
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
	r.mu.Lock()
	defer r.mu.Unlock()
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
	all, err := r.scan()
	if err != nil {
		return StoredArtifact{}, err
	}
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
	if err := r.validateIntegrity(artifact, current.Path, all); err != nil {
		return StoredArtifact{}, err
	}
	raw, err := marshal(artifact)
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
		return StoredArtifact{}, err
	}
	if err := r.commit(ctx, current.file, commitMessage(artifact, operation)); err != nil {
		_ = os.WriteFile(current.file, current.raw, 0o600)
		_, _ = r.git(ctx, "reset", "--quiet", "HEAD", "--", relative(r.root, current.file))
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
				if json.Unmarshal(value, &fields) != nil {
					return fail(400, "invalid_value", "Fields are invalid.")
				}
				artifact.Fields = fields
			}
		case "content":
			if remove {
				artifact.Content = nil
			} else if json.Unmarshal(value, &artifact.Content) != nil {
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

func (r *Repository) Delete(ctx context.Context, guid, expectedETag string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	all, err := r.scan()
	if err != nil {
		return err
	}
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
		schema, err := r.EffectiveSchema(artifact.Type, item.Path)
		if err != nil {
			return err
		}
		for _, reference := range fieldReferences(resolvedFields(artifact, all), object(schema["fields"])) {
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
		return err
	}
	if err := r.commit(ctx, directory, commitMessage(current.Artifact, "deleted")); err != nil {
		_ = os.MkdirAll(directory, 0o700)
		_ = os.WriteFile(current.file, current.raw, 0o600)
		_, _ = r.git(ctx, "reset", "--quiet", "HEAD", "--", relative(r.root, directory))
		return err
	}
	return nil
}

func (r *Repository) validateIntegrity(artifact Artifact, folder string, all map[string]StoredArtifact) error {
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
	schema, err := r.EffectiveSchema(artifact.Type, folder)
	if err != nil {
		return err
	}
	definitions := object(schema["fields"])
	fields := resolvedFields(artifact, all)
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
	for id, state := range artifact.Workflows {
		if !assigned[id] {
			return fail(400, "workflow_not_assigned", "Workflow '"+id+"' is not assigned by the schema.")
		}
		definition, err := r.readWorkflow(id, folder)
		if err != nil {
			return err
		}
		if !slices.Contains(definition.States, state) {
			return fail(400, "invalid_workflow_state", "Workflow state is invalid.")
		}
	}
	return nil
}

func resolvedFields(artifact Artifact, all map[string]StoredArtifact) map[string]any {
	chain := []Artifact{artifact}
	for cursor := artifact; cursor.Base != ""; {
		base, ok := all[strings.ToLower(cursor.Base)]
		if !ok {
			break
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
	return result
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
		raw, exists, err := r.headFile(context.Background(), name)
		if !exists {
			continue
		}
		if err != nil {
			return nil, err
		}
		var local map[string]any
		if json.Unmarshal(raw, &local) != nil {
			return nil, fail(500, "repository_corrupt", "Invalid schema "+name+".")
		}
		if local["inheritance"] == "off" {
			effective = map[string]any{"id": artifactType, "fields": map[string]any{}}
		}
		effective = merge(effective, local)
	}
	return effective, nil
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
		if maximum, ok := definition["maxLength"].(float64); ok && len(text) > int(maximum) {
			return fail(400, "invalid_field", "Field '"+name+"' is too long.")
		}
	}
	if fieldType == "boolean" {
		if _, ok := value.(bool); !ok {
			return fail(400, "invalid_field", "Field '"+name+"' must be boolean.")
		}
	}
	if slices.Contains([]string{"number", "float", "currency"}, fieldType) {
		if _, ok := value.(float64); !ok {
			return fail(400, "invalid_field", "Field '"+name+"' must be a number.")
		}
	}
	if fieldType == "integer" {
		number, ok := value.(float64)
		if !ok || number != float64(int64(number)) {
			return fail(400, "invalid_field", "Field '"+name+"' must be an integer.")
		}
	}
	if fieldType == "enumeration" {
		values, _ := definition["values"].([]any)
		if len(values) > 0 && !slices.ContainsFunc(values, func(allowed any) bool { return fmt.Sprint(allowed) == fmt.Sprint(value) }) {
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

func (r *Repository) ResolveOverlay(guid string) (Overlay, error) {
	all, err := r.scan()
	if err != nil {
		return Overlay{}, err
	}
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
	links, err := r.List(Filters{Kind: Link})
	if err != nil {
		return Links{}, err
	}
	result := Links{Incoming: []StoredArtifact{}, Outgoing: []StoredArtifact{}}
	for _, item := range links {
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
	items, err := r.List(Filters{Kind: input.Kind, Type: input.Type})
	if err != nil {
		return nil, err
	}
	result := make([]StoredArtifact, 0, min(input.Limit, len(items)))
	for _, item := range items {
		raw, _ := json.Marshal(item.Artifact)
		if query == "" || strings.Contains(strings.ToLower(string(raw)), query) {
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
	items, err := r.List(Filters{})
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

func (r *Repository) readWorkflow(id, folder string) (workflow, error) {
	if err := validateIdentifier(id, "Workflow"); err != nil {
		return workflow{}, err
	}
	parts := strings.Split(folder, "/")
	if folder == "" {
		parts = nil
	}
	for index := len(parts); index >= 0; index-- {
		name := pathpkg.Join(pathpkg.Join(parts[:index]...), ".origoa", "workflows", id+".json")
		raw, exists, err := r.headFile(context.Background(), name)
		if !exists {
			continue
		}
		if err != nil {
			return workflow{}, err
		}
		var value workflow
		if json.Unmarshal(raw, &value) != nil || value.ID != id || !identifierPattern.MatchString(value.Initial) || !slices.Contains(value.States, value.Initial) {
			return workflow{}, fail(500, "repository_corrupt", "Invalid workflow "+name+".")
		}
		for _, transition := range value.Transitions {
			if !identifierPattern.MatchString(transition.ID) || !slices.Contains(value.States, transition.From) || !slices.Contains(value.States, transition.To) {
				return workflow{}, fail(500, "repository_corrupt", "Workflow contains an invalid transition.")
			}
		}
		return value, nil
	}
	return workflow{}, fail(404, "workflow_not_found", "Workflow not found.")
}

func (r *Repository) Workflows(guid string) ([]WorkflowInfo, error) {
	item, err := r.Get(guid)
	if err != nil {
		return nil, err
	}
	schema, err := r.EffectiveSchema(item.Artifact.Type, item.Path)
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
		definition, err := r.readWorkflow(id, item.Path)
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
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := validateIdentifier(workflowID, "Workflow"); err != nil {
		return StoredArtifact{}, err
	}
	if err := validateIdentifier(transitionID, "Transition"); err != nil {
		return StoredArtifact{}, err
	}
	all, err := r.scan()
	if err != nil {
		return StoredArtifact{}, err
	}
	current, err := r.get(guid, all)
	if err != nil {
		return StoredArtifact{}, err
	}
	if current.ETag != expectedETag {
		return StoredArtifact{}, fail(412, "version_conflict", "Artifact has changed; reload before saving.")
	}
	schema, err := r.EffectiveSchema(current.Artifact.Type, current.Path)
	if err != nil {
		return StoredArtifact{}, err
	}
	if !configuredWorkflows(schema)[workflowID] {
		return StoredArtifact{}, fail(409, "workflow_not_assigned", "Workflow is not assigned by the schema.")
	}
	definition, err := r.readWorkflow(workflowID, current.Path)
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

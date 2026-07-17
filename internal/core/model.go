// Package core implements the Origoa Foundation: schema-driven artifacts
// stored in Git with a rebuildable in-memory projection.
package core

import (
	"crypto/rand"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
)

const (
	KindEntry    = "entry"
	KindDocument = "document"
	KindLink     = "link"
	KindComment  = "comment"

	MetaDir      = ".origoa"
	ArtifactFile = ".origoa.json"
)

var (
	ErrNotFound  = errors.New("not found")
	ErrConflict  = errors.New("conflict")
	ErrValidation = errors.New("validation")
)

func vErr(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrValidation, fmt.Sprintf(format, a...))
}

var guidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// NewGUID returns a random UUIDv4.
func NewGUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func IsGUID(s string) bool { return guidRe.MatchString(s) }

// Meta is the projected summary of one artifact.
type Meta struct {
	GUID      string            `json:"guid"`
	Kind      string            `json:"kind"`
	Type      string            `json:"type"`
	Title     string            `json:"title,omitempty"`
	HID       string            `json:"hid,omitempty"`
	Base      string            `json:"base,omitempty"`
	Source    string            `json:"source,omitempty"`
	Target    string            `json:"target,omitempty"`
	Subject   string            `json:"subject,omitempty"`
	Workflows map[string]string `json:"workflows,omitempty"`
	// FilePath is the repository path of the artifact's JSON file.
	FilePath string `json:"filePath"`
	// Folder is the organizational folder containing the artifact.
	Folder string `json:"folder"`
	// ETag is the Git blob SHA of the artifact file (optimistic concurrency).
	ETag string `json:"etag"`
}

// CleanFolder validates and normalizes an organizational folder path.
// "" is the repository root. Rejects traversal, metadata directories and
// GUID segments (artifacts cannot nest inside other artifacts).
func CleanFolder(p string) (string, error) {
	p = strings.Trim(strings.TrimSpace(p), "/")
	if p == "" {
		return "", nil
	}
	if strings.ContainsAny(p, "\\\x00") {
		return "", vErr("invalid folder path %q", p)
	}
	if path.Clean(p) != p {
		return "", vErr("folder path %q is not normalized", p)
	}
	for _, seg := range strings.Split(p, "/") {
		switch {
		case seg == "" || seg == "." || seg == "..":
			return "", vErr("invalid folder segment %q", seg)
		case seg == MetaDir:
			return "", vErr("folder path may not contain %q", MetaDir)
		case IsGUID(seg):
			return "", vErr("folder path may not contain artifact directories")
		}
	}
	return p, nil
}

// metaScope returns the .origoa directory used for metadata attached to an
// artifact stored in folder (metadata locality).
func metaScope(folder string) string {
	if folder == "" {
		return MetaDir
	}
	return folder + "/" + MetaDir
}

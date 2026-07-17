package core

import (
	"encoding/json"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/thomdehoog/origoa/internal/ojson"
)

// Projection is the derived query layer over the Git repository. It is never
// authoritative: every implementation must be fully reconstructable from the
// repository via Sync. Two implementations exist — an in-memory one (default,
// zero dependencies) and a PostgreSQL one (persistent, per the design guide).
type Projection interface {
	// Head returns the Git revision the projection represents ("" if none).
	Head() string
	// Sync rebuilds the entire projection from the repository HEAD.
	Sync() error
	// Apply projects the changes of one published commit.
	Apply(newHead string, changes []Change) error

	Get(guid string) (*Meta, bool)
	List(q ListQuery) []*Meta
	LinksFor(guid string) (incoming, outgoing []*Meta)
	CommentsFor(subject string) []*Meta
	HIDOwner(hid string) (string, bool)
	MaxHIDNumber(prefix string) int
	Folders() []string
	// SchemaDefs returns definitions for typ in the given scopes, ordered
	// root -> leaf (composition order).
	SchemaDefs(typ string, scopes []string) []*Schema
	SchemasByScope() map[string][]*Schema
	// Workflow resolves a workflow definition; the nearest scope wins.
	Workflow(id string, scopes []string) *Workflow

	Close() error
}

// Change is one file-level repository change to project.
type Change struct {
	Path    string
	SHA     string
	Content []byte // nil for deletions
	Delete  bool
}

// ListQuery filters artifact listings. Text is a case-insensitive search
// term; empty matches everything.
type ListQuery struct {
	Kind    string
	Type    string
	Folder  string
	Subtree bool
	Text    string
}

func (q ListQuery) matches(m *Meta, searchText string) bool {
	if q.Kind != "" && m.Kind != q.Kind {
		return false
	}
	if q.Type != "" && m.Type != q.Type {
		return false
	}
	if q.Folder != "" || !q.Subtree {
		if q.Subtree {
			if m.Folder != q.Folder && !strings.HasPrefix(m.Folder, q.Folder+"/") {
				return false
			}
		} else if m.Folder != q.Folder {
			return false
		}
	}
	return q.Text == "" || strings.Contains(searchText, strings.ToLower(q.Text))
}

// record is the classified content of one repository file.
type record struct {
	filePath string
	meta     *Meta   // set for artifacts of any kind
	text     string  // searchable text for artifacts
	scope    string  // set for configuration files
	category string  // "schema" | "workflow"
	schema   *Schema
	workflow *Workflow
	raw      json.RawMessage
}

// classify inspects one repository file and returns its projected record, or
// nil if the file is irrelevant or malformed. The projection must tolerate
// arbitrary direct Git modifications, so malformed files are skipped, never
// fatal.
func classify(filePath, sha string, content []byte) *record {
	dir, base := path.Split(filePath)
	dir = strings.TrimSuffix(dir, "/")

	switch {
	case base == ArtifactFile && IsGUID(path.Base(dir)):
		obj, err := ojson.Parse(content)
		if err != nil {
			return nil
		}
		m := &Meta{
			GUID:     obj.GetString("guid"),
			Kind:     obj.GetString("kind"),
			Type:     obj.GetString("type"),
			Title:    obj.GetString("title"),
			HID:      obj.GetString("hid"),
			Base:     obj.GetString("base"),
			FilePath: filePath,
			Folder:   parentFolder(dir),
			ETag:     sha,
		}
		if m.GUID != path.Base(dir) || (m.Kind != KindEntry && m.Kind != KindDocument) {
			return nil
		}
		if raw, ok := obj.Get("workflows"); ok {
			_ = json.Unmarshal(raw, &m.Workflows)
		}
		return &record{filePath: filePath, meta: m, text: searchText(obj)}

	case strings.Contains("/"+filePath, "/"+MetaDir+"/links/"):
		return classifyMetaArtifact(filePath, sha, content, KindLink)

	case strings.Contains("/"+filePath, "/"+MetaDir+"/comments/"):
		return classifyMetaArtifact(filePath, sha, content, KindComment)

	case strings.Contains("/"+filePath, "/"+MetaDir+"/schemas/"):
		var s Schema
		if err := json.Unmarshal(content, &s); err != nil || s.ArtifactType == "" {
			return nil
		}
		return &record{filePath: filePath, scope: scopeOf(filePath), category: "schema", schema: &s, raw: content}

	case strings.Contains("/"+filePath, "/"+MetaDir+"/workflows/"):
		var w Workflow
		if err := json.Unmarshal(content, &w); err != nil || w.ID == "" {
			return nil
		}
		return &record{filePath: filePath, scope: scopeOf(filePath), category: "workflow", workflow: &w, raw: content}
	}
	return nil
}

func classifyMetaArtifact(filePath, sha string, content []byte, kind string) *record {
	obj, err := ojson.Parse(content)
	if err != nil || obj.GetString("kind") != kind {
		return nil
	}
	m := &Meta{
		GUID:     obj.GetString("guid"),
		Kind:     kind,
		Type:     obj.GetString("type"),
		Source:   obj.GetString("source"),
		Target:   obj.GetString("target"),
		Subject:  obj.GetString("subject"),
		FilePath: filePath,
		Folder:   scopeOf(filePath),
		ETag:     sha,
	}
	if !IsGUID(m.GUID) {
		return nil
	}
	return &record{filePath: filePath, meta: m, text: searchText(obj)}
}

// scopeOf maps ".../<scope>/.origoa/xxx/file.json" to "<scope>".
func scopeOf(filePath string) string {
	i := strings.LastIndex("/"+filePath, "/"+MetaDir+"/")
	if i <= 0 {
		return ""
	}
	return filePath[:i-1]
}

func parentFolder(dir string) string {
	p := path.Dir(dir)
	if p == "." {
		return ""
	}
	return p
}

// searchText flattens an artifact object into lowercase text for search.
func searchText(obj *ojson.Obj) string {
	var parts []string
	var walk func(raw json.RawMessage)
	walk = func(raw json.RawMessage) {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			parts = append(parts, s)
			return
		}
		var arr []json.RawMessage
		if json.Unmarshal(raw, &arr) == nil {
			for _, v := range arr {
				walk(v)
			}
			return
		}
		var m map[string]json.RawMessage
		if json.Unmarshal(raw, &m) == nil {
			for _, v := range m {
				walk(v)
			}
		}
	}
	for _, k := range obj.Keys() {
		if k == "guid" || k == "base" || k == "kind" {
			continue
		}
		raw, _ := obj.Get(k)
		walk(raw)
	}
	return strings.ToLower(strings.Join(parts, " "))
}

// withAncestors expands a set of folders with all their ancestors, sorted.
func withAncestors(folders map[string]bool) []string {
	all := map[string]bool{}
	for f := range folders {
		for f != "" {
			all[f] = true
			f = parentFolder(f)
		}
	}
	out := make([]string, 0, len(all))
	for f := range all {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// hidNumber extracts N from "<prefix>-<N>".
func hidNumber(hid, prefix string) (int, bool) {
	rest, ok := strings.CutPrefix(hid, prefix+"-")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	return n, err == nil
}

// escapeLike escapes SQL LIKE wildcards (default backslash escape).
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

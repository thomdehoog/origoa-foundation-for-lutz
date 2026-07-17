package core

import (
	"encoding/json"
	"path"
	"sort"
	"strings"
	"sync"

	"github.com/thomdehoog/origoa/internal/gitx"
	"github.com/thomdehoog/origoa/internal/ojson"
)

// index is the in-memory repository projection: GUID resolution, hierarchy,
// metadata, schemas, workflows and searchable text. It is derived state and
// is rebuilt entirely from the Git HEAD tree.
type index struct {
	mu        sync.RWMutex
	head      string
	byGUID    map[string]*Meta
	byHID     map[string]string   // HID -> GUID
	text      map[string]string   // GUID -> lowercase searchable text
	schemas   map[string][]*Schema          // scope dir -> definitions
	workflows map[string]map[string]*Workflow // scope dir -> id -> definition
	folders   map[string]bool
}

func newIndex() *index {
	return &index{
		byGUID:    map[string]*Meta{},
		byHID:     map[string]string{},
		text:      map[string]string{},
		schemas:   map[string][]*Schema{},
		workflows: map[string]map[string]*Workflow{},
		folders:   map[string]bool{},
	}
}

// rebuild reconstructs the whole projection from the repository HEAD.
func (ix *index) rebuild(g *gitx.Repo) error {
	head, err := g.Head()
	if err != nil {
		return err
	}
	entries, err := g.ListTree(head, "")
	if err != nil {
		return err
	}
	shas := make([]string, 0, len(entries))
	for _, e := range entries {
		shas = append(shas, e.SHA)
	}
	blobs, err := g.ReadBlobs(shas)
	if err != nil {
		return err
	}

	fresh := newIndex()
	fresh.head = head
	for _, e := range entries {
		fresh.ingest(e.Path, e.SHA, blobs[e.SHA])
	}

	ix.mu.Lock()
	ix.head, ix.byGUID, ix.byHID, ix.text = fresh.head, fresh.byGUID, fresh.byHID, fresh.text
	ix.schemas, ix.workflows, ix.folders = fresh.schemas, fresh.workflows, fresh.folders
	ix.mu.Unlock()
	return nil
}

// ingest classifies one repository file and updates the projection.
// Malformed files are skipped: the projection must tolerate arbitrary direct
// Git modifications.
func (ix *index) ingest(filePath, sha string, content []byte) {
	dir, base := path.Split(filePath)
	dir = strings.TrimSuffix(dir, "/")

	switch {
	case base == ArtifactFile && IsGUID(path.Base(dir)):
		obj, err := ojson.Parse(content)
		if err != nil {
			return
		}
		m := &Meta{
			GUID:     obj.GetString("guid"),
			Kind:     obj.GetString("kind"),
			Type:     obj.GetString("type"),
			Title:    obj.GetString("title"),
			HID:      obj.GetString("hid"),
			Base:     obj.GetString("base"),
			FilePath: filePath,
			Folder:   path.Dir(dir),
			ETag:     sha,
		}
		if m.Folder == "." {
			m.Folder = ""
		}
		if m.GUID != path.Base(dir) || (m.Kind != KindEntry && m.Kind != KindDocument) {
			return
		}
		if raw, ok := obj.Get("workflows"); ok {
			_ = json.Unmarshal(raw, &m.Workflows)
		}
		ix.addMeta(m, searchText(obj))

	case strings.Contains("/"+filePath, "/"+MetaDir+"/links/"):
		ix.ingestMetaArtifact(filePath, sha, content, KindLink)

	case strings.Contains("/"+filePath, "/"+MetaDir+"/comments/"):
		ix.ingestMetaArtifact(filePath, sha, content, KindComment)

	case strings.Contains("/"+filePath, "/"+MetaDir+"/schemas/"):
		var s Schema
		if err := json.Unmarshal(content, &s); err != nil || s.ArtifactType == "" {
			return
		}
		scope := scopeOf(filePath)
		ix.schemas[scope] = append(ix.schemas[scope], &s)
		ix.addFolders(scope)

	case strings.Contains("/"+filePath, "/"+MetaDir+"/workflows/"):
		var w Workflow
		if err := json.Unmarshal(content, &w); err != nil || w.ID == "" {
			return
		}
		scope := scopeOf(filePath)
		if ix.workflows[scope] == nil {
			ix.workflows[scope] = map[string]*Workflow{}
		}
		ix.workflows[scope][w.ID] = &w
		ix.addFolders(scope)
	}
}

func (ix *index) ingestMetaArtifact(filePath, sha string, content []byte, kind string) {
	obj, err := ojson.Parse(content)
	if err != nil || obj.GetString("kind") != kind {
		return
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
		return
	}
	ix.addMeta(m, searchText(obj))
}

// scopeOf maps ".../<scope>/.origoa/xxx/file.json" to "<scope>".
func scopeOf(filePath string) string {
	i := strings.LastIndex("/"+filePath, "/"+MetaDir+"/")
	if i <= 0 {
		return ""
	}
	return filePath[:i-1]
}

func (ix *index) addMeta(m *Meta, text string) {
	ix.byGUID[m.GUID] = m
	if m.HID != "" {
		ix.byHID[m.HID] = m.GUID
	}
	ix.text[m.GUID] = text
	ix.addFolders(m.Folder)
}

func (ix *index) addFolders(folder string) {
	for folder != "" {
		ix.folders[folder] = true
		folder = path.Dir(folder)
		if folder == "." {
			folder = ""
		}
	}
}

func (ix *index) removeMeta(guid string) {
	m, ok := ix.byGUID[guid]
	if !ok {
		return
	}
	delete(ix.byGUID, guid)
	delete(ix.text, guid)
	if m.HID != "" && ix.byHID[m.HID] == guid {
		delete(ix.byHID, m.HID)
	}
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

// effectiveSchema composes the schema for an artifact type at a folder.
func (ix *index) effectiveSchema(typ, folder string) *Schema {
	var defs []*Schema
	for _, scope := range scopeChain(folder) {
		for _, s := range ix.schemas[scope] {
			if s.ArtifactType == typ {
				defs = append(defs, s)
			}
		}
	}
	if len(defs) == 0 {
		return nil
	}
	return composeSchemas(defs)
}

// resolveWorkflow finds the nearest workflow definition for id at folder.
func (ix *index) resolveWorkflow(id, folder string) *Workflow {
	scopes := scopeChain(folder)
	for i := len(scopes) - 1; i >= 0; i-- {
		if w, ok := ix.workflows[scopes[i]][id]; ok {
			return w
		}
	}
	return nil
}

func (ix *index) sortedMetas(filter func(*Meta) bool) []*Meta {
	var out []*Meta
	for _, m := range ix.byGUID {
		if filter == nil || filter(m) {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].FilePath != out[j].FilePath {
			return out[i].FilePath < out[j].FilePath
		}
		return out[i].GUID < out[j].GUID
	})
	return out
}

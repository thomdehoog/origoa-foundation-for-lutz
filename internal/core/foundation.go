package core

import (
	"encoding/json"
	"fmt"
	"maps"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/thomdehoog/origoa/internal/gitx"
	"github.com/thomdehoog/origoa/internal/ojson"
)

// Foundation exposes all repository operations. Git is the single source of
// truth; every write produces exactly one commit describing the logical
// operation, after which the projection is rebuilt from HEAD ("never trust
// metadata when the primary data already exists").
type Foundation struct {
	git *gitx.Repo
	ix  *index
	wmu sync.Mutex // serializes writers
}

var typeRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*$`)

// Open initializes (if needed) the bare repository at gitDir and builds the
// projection.
func Open(gitDir string) (*Foundation, error) {
	g, err := gitx.Init(gitDir)
	if err != nil {
		return nil, err
	}
	f := &Foundation{git: g, ix: newIndex()}
	if err := f.Reindex(); err != nil {
		return nil, err
	}
	return f, nil
}

// Reindex rebuilds all derived state from the Git HEAD.
func (f *Foundation) Reindex() error { return f.ix.rebuild(f.git) }

// Head returns the Git revision the projection represents.
func (f *Foundation) Head() string {
	f.ix.mu.RLock()
	defer f.ix.mu.RUnlock()
	return f.ix.head
}

// commit publishes ops and synchronizes the projection.
func (f *Foundation) commit(msg string, ops []gitx.Op) error {
	if _, err := f.git.Commit(msg, ops); err != nil {
		return err
	}
	return f.ix.rebuild(f.git)
}

// ---- reads ----

func (f *Foundation) Meta(guid string) (*Meta, error) {
	f.ix.mu.RLock()
	defer f.ix.mu.RUnlock()
	m, ok := f.ix.byGUID[guid]
	if !ok {
		return nil, fmt.Errorf("%w: artifact %s", ErrNotFound, guid)
	}
	cp := *m
	cp.Workflows = maps.Clone(m.Workflows) // callers may mutate; never alias the projection
	return &cp, nil
}

// Artifact returns the projected meta and the full stored object.
func (f *Foundation) Artifact(guid string) (*Meta, *ojson.Obj, error) {
	m, err := f.Meta(guid)
	if err != nil {
		return nil, nil, err
	}
	blobs, err := f.git.ReadBlobs([]string{m.ETag})
	if err != nil {
		return nil, nil, err
	}
	obj, err := ojson.Parse(blobs[m.ETag])
	if err != nil {
		return nil, nil, err
	}
	return m, obj, nil
}

// List returns artifact metas filtered by kind, type and folder. With
// subtree, artifacts in nested folders are included.
func (f *Foundation) List(kind, typ, folder string, subtree bool) []*Meta {
	f.ix.mu.RLock()
	defer f.ix.mu.RUnlock()
	return f.ix.sortedMetas(func(m *Meta) bool {
		if kind != "" && m.Kind != kind {
			return false
		}
		if typ != "" && m.Type != typ {
			return false
		}
		if folder != "" || !subtree {
			if subtree {
				if m.Folder != folder && !strings.HasPrefix(m.Folder, folder+"/") {
					return false
				}
			} else if m.Folder != folder {
				return false
			}
		}
		return true
	})
}

func (f *Foundation) Folders() []string {
	f.ix.mu.RLock()
	defer f.ix.mu.RUnlock()
	out := make([]string, 0, len(f.ix.folders))
	for folder := range f.ix.folders {
		out = append(out, folder)
	}
	sort.Strings(out)
	return out
}

func (f *Foundation) Search(q, kind, typ string) []*Meta {
	q = strings.ToLower(strings.TrimSpace(q))
	f.ix.mu.RLock()
	defer f.ix.mu.RUnlock()
	return f.ix.sortedMetas(func(m *Meta) bool {
		if kind != "" && m.Kind != kind {
			return false
		}
		if typ != "" && m.Type != typ {
			return false
		}
		return q == "" || strings.Contains(f.ix.text[m.GUID], q)
	})
}

func (f *Foundation) EffectiveSchema(typ, folder string) (*Schema, error) {
	if folder != "" {
		var err error
		if folder, err = CleanFolder(folder); err != nil {
			return nil, err
		}
	}
	f.ix.mu.RLock()
	defer f.ix.mu.RUnlock()
	s := f.ix.effectiveSchema(typ, folder)
	if s == nil {
		return nil, fmt.Errorf("%w: no schema for type %q", ErrNotFound, typ)
	}
	return s, nil
}

// Schemas returns all schema definitions grouped by configuration scope.
func (f *Foundation) Schemas() map[string][]*Schema {
	f.ix.mu.RLock()
	defer f.ix.mu.RUnlock()
	out := map[string][]*Schema{}
	for scope, defs := range f.ix.schemas {
		out[scope] = append([]*Schema(nil), defs...)
	}
	return out
}

func (f *Foundation) WorkflowDef(id, folder string) (*Workflow, error) {
	f.ix.mu.RLock()
	defer f.ix.mu.RUnlock()
	w := f.ix.resolveWorkflow(id, folder)
	if w == nil {
		return nil, fmt.Errorf("%w: workflow %q", ErrNotFound, id)
	}
	return w, nil
}

// ResolveOverlay merges the fields of an entry with its base chain.
// The overlay's own fields win; unresolved fields come from the nearest base.
func (f *Foundation) ResolveOverlay(guid string) (fields map[string]json.RawMessage, chain []string, err error) {
	fields = map[string]json.RawMessage{}
	visited := map[string]bool{}
	current := guid
	for current != "" {
		if visited[current] {
			return nil, nil, vErr("overlay cycle at %s", current)
		}
		visited[current] = true
		chain = append(chain, current)
		m, obj, err := f.Artifact(current)
		if err != nil {
			return nil, nil, err
		}
		if m.Kind != KindEntry {
			return nil, nil, vErr("overlay base %s is not an entry", current)
		}
		if raw, ok := obj.Get("fields"); ok {
			var fm map[string]json.RawMessage
			if json.Unmarshal(raw, &fm) == nil {
				for k, v := range fm {
					if _, done := fields[k]; !done {
						fields[k] = v
					}
				}
			}
		}
		current = m.Base
	}
	return fields, chain, nil
}

// Links returns incoming and outgoing links of an artifact.
func (f *Foundation) Links(guid string) (incoming, outgoing []*Meta) {
	f.ix.mu.RLock()
	defer f.ix.mu.RUnlock()
	for _, m := range f.ix.sortedMetas(func(m *Meta) bool { return m.Kind == KindLink }) {
		if m.Target == guid {
			incoming = append(incoming, m)
		}
		if m.Source == guid {
			outgoing = append(outgoing, m)
		}
	}
	return
}

// Comments returns all comments whose subject is guid.
func (f *Foundation) Comments(guid string) []*Meta {
	f.ix.mu.RLock()
	defer f.ix.mu.RUnlock()
	return f.ix.sortedMetas(func(m *Meta) bool {
		return m.Kind == KindComment && m.Subject == guid
	})
}

// History returns the commit log touching an artifact.
func (f *Foundation) History(guid string, limit int) ([]gitx.LogEntry, error) {
	m, err := f.Meta(guid)
	if err != nil {
		return nil, err
	}
	p := m.FilePath
	if m.Kind == KindEntry || m.Kind == KindDocument {
		p = path.Dir(m.FilePath)
	}
	return f.git.Log(p, limit)
}

// ---- writes ----

// CreateArtifact creates an entry or document. body may provide title, hid,
// base, fields and (for documents) content.
func (f *Foundation) CreateArtifact(kind, folder, typ string, body *ojson.Obj) (*Meta, error) {
	if kind != KindEntry && kind != KindDocument {
		return nil, vErr("invalid kind %q", kind)
	}
	folder, err := CleanFolder(folder)
	if err != nil {
		return nil, err
	}
	if !typeRe.MatchString(typ) {
		return nil, vErr("invalid artifact type %q", typ)
	}
	f.wmu.Lock()
	defer f.wmu.Unlock()

	guid := NewGUID()
	obj := ojson.New()
	obj.SetString("guid", guid)
	obj.SetString("kind", kind)
	obj.SetString("type", typ)

	schema := func() *Schema {
		f.ix.mu.RLock()
		defer f.ix.mu.RUnlock()
		return f.ix.effectiveSchema(typ, folder)
	}()

	hid := strings.TrimSpace(body.GetString("hid"))
	if hid == "" && schema != nil && schema.HIDPrefix != "" {
		hid = f.nextHID(schema.HIDPrefix)
	}
	if hid != "" {
		if err := f.checkHID(hid, guid); err != nil {
			return nil, err
		}
		obj.SetString("hid", hid)
	}
	obj.SetString("title", strings.TrimSpace(body.GetString("title")))

	if base := body.GetString("base"); base != "" {
		if kind != KindEntry {
			return nil, vErr("only entries support overlays")
		}
		if err := f.checkBase(base, guid); err != nil {
			return nil, err
		}
		obj.SetString("base", base)
	}

	if schema != nil && len(schema.Workflows) > 0 {
		states := map[string]string{}
		for _, wfID := range schema.Workflows {
			if wf := f.resolveWorkflowLocked(wfID, folder); wf != nil {
				states[wfID] = wf.Initial
			}
		}
		if len(states) > 0 {
			obj.SetAny("workflows", states)
		}
	}

	if raw, ok := body.Get("fields"); ok {
		if err := requireObject(raw, "fields"); err != nil {
			return nil, err
		}
		obj.Set("fields", raw)
	}
	if raw, ok := body.Get("content"); ok && kind == KindDocument {
		obj.Set("content", raw)
	}

	filePath := path.Join(folderOrRoot(folder), guid, ArtifactFile)
	content, err := obj.Encode()
	if err != nil {
		return nil, err
	}
	msg := fmt.Sprintf("%s %s in /%s created", titleKind(kind), guid, folder)
	if err := f.commit(msg, []gitx.Op{{Path: filePath, Content: content}}); err != nil {
		return nil, err
	}
	return f.Meta(guid)
}

var updatableKeys = map[string]bool{"title": true, "hid": true, "base": true, "fields": true, "content": true}

// UpdateArtifact patches the mutable properties of an entry or document.
// ifMatch, when non-empty, must equal the artifact's current ETag.
func (f *Foundation) UpdateArtifact(guid string, patch *ojson.Obj, ifMatch string) (*Meta, error) {
	f.wmu.Lock()
	defer f.wmu.Unlock()

	m, obj, err := f.Artifact(guid)
	if err != nil {
		return nil, err
	}
	if m.Kind != KindEntry && m.Kind != KindDocument {
		return nil, vErr("%s artifacts are immutable; delete and recreate", m.Kind)
	}
	if ifMatch != "" && ifMatch != m.ETag {
		return nil, fmt.Errorf("%w: artifact was modified concurrently", ErrConflict)
	}
	for _, k := range patch.Keys() {
		if !updatableKeys[k] {
			return nil, vErr("property %q cannot be updated", k)
		}
		if k == "content" && m.Kind != KindDocument {
			return nil, vErr("only documents have content")
		}
	}
	if patch.Has("hid") {
		hid := strings.TrimSpace(patch.GetString("hid"))
		if hid == "" {
			obj.Delete("hid")
		} else if hid != m.HID {
			if err := f.checkHID(hid, guid); err != nil {
				return nil, err
			}
			obj.SetString("hid", hid)
		}
	}
	if patch.Has("base") {
		base := patch.GetString("base")
		if base == "" {
			obj.Delete("base")
		} else {
			if m.Kind != KindEntry {
				return nil, vErr("only entries support overlays")
			}
			if err := f.checkBase(base, guid); err != nil {
				return nil, err
			}
			obj.SetString("base", base)
		}
	}
	if patch.Has("title") {
		obj.SetString("title", strings.TrimSpace(patch.GetString("title")))
	}
	if raw, ok := patch.Get("fields"); ok {
		if err := requireObject(raw, "fields"); err != nil {
			return nil, err
		}
		obj.Set("fields", raw)
	}
	if raw, ok := patch.Get("content"); ok {
		obj.Set("content", raw)
	}
	content, err := obj.Encode()
	if err != nil {
		return nil, err
	}
	msg := fmt.Sprintf("%s %s updated", titleKind(m.Kind), guid)
	if err := f.commit(msg, []gitx.Op{{Path: m.FilePath, Content: content}}); err != nil {
		return nil, err
	}
	return f.Meta(guid)
}

// DeleteArtifact removes an artifact of any kind. Links and comments
// referencing it remain valid history and become dangling by design.
func (f *Foundation) DeleteArtifact(guid string) error {
	f.wmu.Lock()
	defer f.wmu.Unlock()

	m, err := f.Meta(guid)
	if err != nil {
		return err
	}
	ops, err := f.artifactOps(m, true, "")
	if err != nil {
		return err
	}
	return f.commit(fmt.Sprintf("%s %s deleted", titleKind(m.Kind), guid), ops)
}

// MoveArtifact relocates an entry or document to another folder. The GUID and
// all references remain unchanged.
func (f *Foundation) MoveArtifact(guid, newFolder string) (*Meta, error) {
	newFolder, err := CleanFolder(newFolder)
	if err != nil {
		return nil, err
	}
	f.wmu.Lock()
	defer f.wmu.Unlock()

	m, err := f.Meta(guid)
	if err != nil {
		return nil, err
	}
	if m.Kind != KindEntry && m.Kind != KindDocument {
		return nil, vErr("only entries and documents can be moved")
	}
	if newFolder == m.Folder {
		return m, nil
	}
	ops, err := f.artifactOps(m, true, newFolder)
	if err != nil {
		return nil, err
	}
	msg := fmt.Sprintf("%s %s moved to /%s", titleKind(m.Kind), guid, newFolder)
	if err := f.commit(msg, ops); err != nil {
		return nil, err
	}
	return f.Meta(guid)
}

// CreateLink creates a typed directed link between two artifacts, stored in
// the metadata scope nearest to the source artifact.
func (f *Foundation) CreateLink(typ, source, target string, fields json.RawMessage) (*Meta, error) {
	if !typeRe.MatchString(typ) {
		return nil, vErr("invalid link type %q", typ)
	}
	f.wmu.Lock()
	defer f.wmu.Unlock()

	src, err := f.Meta(source)
	if err != nil {
		return nil, vErr("source artifact %s not found", source)
	}
	tgt, err := f.Meta(target)
	if err != nil {
		return nil, vErr("target artifact %s not found", target)
	}
	// Schema relationship definitions constrain links when present.
	if schema := f.effectiveSchemaLocked(src.Type, src.Folder); schema != nil && len(schema.Relationships) > 0 {
		var rel *Relationship
		for i := range schema.Relationships {
			if schema.Relationships[i].LinkType == typ {
				rel = &schema.Relationships[i]
				break
			}
		}
		if rel == nil {
			return nil, vErr("link type %q not allowed for source type %q", typ, src.Type)
		}
		if len(rel.TargetTypes) > 0 && !contains(rel.TargetTypes, tgt.Type) {
			return nil, vErr("target type %q not allowed for link type %q", tgt.Type, typ)
		}
		if len(rel.SourceTypes) > 0 && !contains(rel.SourceTypes, src.Type) {
			return nil, vErr("source type %q not allowed for link type %q", src.Type, typ)
		}
	}
	guid := NewGUID()
	obj := ojson.New()
	obj.SetString("guid", guid)
	obj.SetString("kind", KindLink)
	obj.SetString("type", typ)
	obj.SetString("source", source)
	obj.SetString("target", target)
	if len(fields) > 0 {
		if err := requireObject(fields, "fields"); err != nil {
			return nil, err
		}
		obj.Set("fields", fields)
	}
	content, err := obj.Encode()
	if err != nil {
		return nil, err
	}
	filePath := path.Join(metaScope(src.Folder), "links", guid+".json")
	msg := fmt.Sprintf("Link %s from %s to %s created", typ, source, target)
	if err := f.commit(msg, []gitx.Op{{Path: filePath, Content: content}}); err != nil {
		return nil, err
	}
	return f.Meta(guid)
}

// CreateComment attaches a comment to an artifact, optionally replying to a
// parent comment on the same subject.
func (f *Foundation) CreateComment(subject, text, parent, author string) (*Meta, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, vErr("comment text is required")
	}
	f.wmu.Lock()
	defer f.wmu.Unlock()

	subj, err := f.Meta(subject)
	if err != nil {
		return nil, vErr("subject artifact %s not found", subject)
	}
	if parent != "" {
		p, err := f.Meta(parent)
		if err != nil || p.Kind != KindComment || p.Subject != subject {
			return nil, vErr("parent %s is not a comment on the same subject", parent)
		}
	}
	guid := NewGUID()
	obj := ojson.New()
	obj.SetString("guid", guid)
	obj.SetString("kind", KindComment)
	obj.SetString("subject", subject)
	if parent != "" {
		obj.SetString("parent", parent)
	}
	if author != "" {
		obj.SetString("author", author)
	}
	obj.SetString("text", text)
	obj.SetString("created", time.Now().UTC().Format(time.RFC3339))
	content, err := obj.Encode()
	if err != nil {
		return nil, err
	}
	filePath := path.Join(metaScope(subj.Folder), "comments", guid+".json")
	msg := fmt.Sprintf("Comment %s added to %s", guid, subject)
	if err := f.commit(msg, []gitx.Op{{Path: filePath, Content: content}}); err != nil {
		return nil, err
	}
	return f.Meta(guid)
}

// Transition executes a workflow state change on an entry or document.
func (f *Foundation) Transition(guid, workflowID, to string) (*Meta, error) {
	f.wmu.Lock()
	defer f.wmu.Unlock()

	m, obj, err := f.Artifact(guid)
	if err != nil {
		return nil, err
	}
	if m.Kind != KindEntry && m.Kind != KindDocument {
		return nil, vErr("artifact kind %q has no workflows", m.Kind)
	}
	schema := f.effectiveSchemaLocked(m.Type, m.Folder)
	if schema == nil || !contains(schema.Workflows, workflowID) {
		return nil, vErr("workflow %q is not assigned to type %q", workflowID, m.Type)
	}
	wf := f.resolveWorkflowLocked(workflowID, m.Folder)
	if wf == nil {
		return nil, vErr("workflow definition %q not found", workflowID)
	}
	from := m.Workflows[workflowID]
	if from == "" {
		from = wf.Initial
	}
	if !wf.CanTransition(from, to) {
		return nil, vErr("no transition from %q to %q in workflow %q", from, to, workflowID)
	}
	states := m.Workflows
	if states == nil {
		states = map[string]string{}
	}
	states[workflowID] = to
	obj.SetAny("workflows", states)
	content, err := obj.Encode()
	if err != nil {
		return nil, err
	}
	msg := fmt.Sprintf("Workflow transition: Item %s transitioned from %s to %s", guid, from, to)
	if err := f.commit(msg, []gitx.Op{{Path: m.FilePath, Content: content}}); err != nil {
		return nil, err
	}
	return f.Meta(guid)
}

// PutSchema stores a schema definition file in a configuration scope.
func (f *Foundation) PutSchema(scope, name string, s *Schema) error {
	return f.putConfig(scope, "schemas", name, s, s.ArtifactType != "" && typeRe.MatchString(s.ArtifactType),
		fmt.Sprintf("Schema %s in /%s updated", name, scope))
}

// PutWorkflow stores a workflow definition file in a configuration scope.
func (f *Foundation) PutWorkflow(scope, name string, w *Workflow) error {
	valid := w.ID != "" && w.Initial != "" && contains(w.States, w.Initial)
	for _, t := range w.Transitions {
		valid = valid && contains(w.States, t.From) && contains(w.States, t.To)
	}
	return f.putConfig(scope, "workflows", name, w, valid,
		fmt.Sprintf("Workflow %s in /%s updated", name, scope))
}

func (f *Foundation) putConfig(scope, dir, name string, v any, valid bool, msg string) error {
	scope, err := CleanFolder(scope)
	if err != nil {
		return err
	}
	if !typeRe.MatchString(name) {
		return vErr("invalid config name %q", name)
	}
	if !valid {
		return vErr("invalid %s definition", strings.TrimSuffix(dir, "s"))
	}
	content, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	f.wmu.Lock()
	defer f.wmu.Unlock()
	filePath := path.Join(metaScope(scope), dir, name+".json")
	return f.commit(msg, []gitx.Op{{Path: filePath, Content: append(content, '\n')}})
}

// ---- helpers ----

// artifactOps returns delete (and, for moves, re-create) ops for all files of
// an artifact.
func (f *Foundation) artifactOps(m *Meta, del bool, moveTo string) ([]gitx.Op, error) {
	var ops []gitx.Op
	if m.Kind == KindLink || m.Kind == KindComment {
		return []gitx.Op{{Path: m.FilePath, Delete: true}}, nil
	}
	dir := path.Dir(m.FilePath)
	entries, err := f.git.ListTree(f.Head(), dir+"/")
	if err != nil {
		return nil, err
	}
	shas := make([]string, len(entries))
	for i, e := range entries {
		shas[i] = e.SHA
	}
	blobs, err := f.git.ReadBlobs(shas)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if del {
			ops = append(ops, gitx.Op{Path: e.Path, Delete: true})
		}
		if moveTo != "" || !del {
			rel := strings.TrimPrefix(e.Path, dir+"/")
			ops = append(ops, gitx.Op{
				Path:    path.Join(folderOrRoot(moveTo), m.GUID, rel),
				Content: blobs[e.SHA],
			})
		}
	}
	return ops, nil
}

func (f *Foundation) checkHID(hid, guid string) error {
	if len(hid) > 100 || strings.ContainsAny(hid, " \t\n") {
		return vErr("invalid HID %q", hid)
	}
	f.ix.mu.RLock()
	defer f.ix.mu.RUnlock()
	if owner, ok := f.ix.byHID[hid]; ok && owner != guid {
		return fmt.Errorf("%w: HID %q is already assigned to %s", ErrConflict, hid, owner)
	}
	return nil
}

// checkBase validates an overlay base: it must exist, be an entry, and the
// chain from it must not lead back to guid.
func (f *Foundation) checkBase(base, guid string) error {
	seen := map[string]bool{guid: true}
	for base != "" {
		if seen[base] {
			return vErr("overlay cycle via base %s", base)
		}
		seen[base] = true
		m, err := f.Meta(base)
		if err != nil {
			return vErr("base artifact %s not found", base)
		}
		if m.Kind != KindEntry {
			return vErr("base artifact %s is not an entry", base)
		}
		base = m.Base
	}
	return nil
}

func (f *Foundation) nextHID(prefix string) string {
	f.ix.mu.RLock()
	defer f.ix.mu.RUnlock()
	max := 0
	for hid := range f.ix.byHID {
		if rest, ok := strings.CutPrefix(hid, prefix+"-"); ok {
			if n, err := strconv.Atoi(rest); err == nil && n > max {
				max = n
			}
		}
	}
	return fmt.Sprintf("%s-%d", prefix, max+1)
}

func (f *Foundation) effectiveSchemaLocked(typ, folder string) *Schema {
	f.ix.mu.RLock()
	defer f.ix.mu.RUnlock()
	return f.ix.effectiveSchema(typ, folder)
}

func (f *Foundation) resolveWorkflowLocked(id, folder string) *Workflow {
	f.ix.mu.RLock()
	defer f.ix.mu.RUnlock()
	return f.ix.resolveWorkflow(id, folder)
}

func requireObject(raw json.RawMessage, name string) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return vErr("%s must be a JSON object", name)
	}
	return nil
}

func folderOrRoot(folder string) string {
	if folder == "" {
		return "."
	}
	return folder
}

func titleKind(kind string) string {
	return strings.ToUpper(kind[:1]) + kind[1:]
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

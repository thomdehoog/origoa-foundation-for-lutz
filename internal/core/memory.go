package core

import (
	"maps"
	"sort"
	"sync"

	"github.com/thomdehoog/origoa/internal/gitx"
)

// memProjection is the in-memory Projection: zero dependencies, rebuilt from
// the Git HEAD. Apply performs a full rebuild — correct by construction; the
// PostgreSQL projection is the incremental, persistent alternative.
type memProjection struct {
	git *gitx.Repo

	mu        sync.RWMutex
	head      string
	byGUID    map[string]*Meta
	text      map[string]string // GUID -> lowercase searchable text
	byHID     map[string]string // HID -> GUID
	schemas   map[string][]*Schema
	workflows map[string]map[string]*Workflow
	folders   map[string]bool
}

func newMemProjection(g *gitx.Repo) *memProjection {
	return &memProjection{git: g}
}

func (p *memProjection) Head() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.head
}

func (p *memProjection) Sync() error {
	head, err := p.git.Head()
	if err != nil {
		return err
	}
	entries, err := p.git.ListTree(head, "")
	if err != nil {
		return err
	}
	shas := make([]string, len(entries))
	for i, e := range entries {
		shas[i] = e.SHA
	}
	blobs, err := p.git.ReadBlobs(shas)
	if err != nil {
		return err
	}

	byGUID := map[string]*Meta{}
	text := map[string]string{}
	byHID := map[string]string{}
	schemas := map[string][]*Schema{}
	workflows := map[string]map[string]*Workflow{}
	folders := map[string]bool{}
	for _, e := range entries { // ls-tree order is path-sorted: deterministic
		rec := classify(e.Path, e.SHA, blobs[e.SHA])
		switch {
		case rec == nil:
		case rec.meta != nil:
			byGUID[rec.meta.GUID] = rec.meta
			text[rec.meta.GUID] = rec.text
			if rec.meta.HID != "" {
				byHID[rec.meta.HID] = rec.meta.GUID
			}
			folders[rec.meta.Folder] = true
		case rec.category == "schema":
			schemas[rec.scope] = append(schemas[rec.scope], rec.schema)
			folders[rec.scope] = true
		case rec.category == "workflow":
			if workflows[rec.scope] == nil {
				workflows[rec.scope] = map[string]*Workflow{}
			}
			workflows[rec.scope][rec.workflow.ID] = rec.workflow
			folders[rec.scope] = true
		}
	}

	p.mu.Lock()
	p.head, p.byGUID, p.text, p.byHID = head, byGUID, text, byHID
	p.schemas, p.workflows, p.folders = schemas, workflows, folders
	p.mu.Unlock()
	return nil
}

func (p *memProjection) Apply(string, []Change) error { return p.Sync() }

func (p *memProjection) Get(guid string) (*Meta, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	m, ok := p.byGUID[guid]
	if !ok {
		return nil, false
	}
	cp := *m
	cp.Workflows = maps.Clone(m.Workflows) // callers may mutate
	return &cp, true
}

func (p *memProjection) List(q ListQuery) []*Meta {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.collect(func(m *Meta) bool { return q.matches(m, p.text[m.GUID]) })
}

func (p *memProjection) LinksFor(guid string) (incoming, outgoing []*Meta) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, m := range p.collect(func(m *Meta) bool { return m.Kind == KindLink }) {
		if m.Target == guid {
			incoming = append(incoming, m)
		}
		if m.Source == guid {
			outgoing = append(outgoing, m)
		}
	}
	return
}

func (p *memProjection) CommentsFor(subject string) []*Meta {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.collect(func(m *Meta) bool { return m.Kind == KindComment && m.Subject == subject })
}

func (p *memProjection) HIDOwner(hid string) (string, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	guid, ok := p.byHID[hid]
	return guid, ok
}

func (p *memProjection) MaxHIDNumber(prefix string) int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	max := 0
	for hid := range p.byHID {
		if n, ok := hidNumber(hid, prefix); ok && n > max {
			max = n
		}
	}
	return max
}

func (p *memProjection) Folders() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return withAncestors(p.folders)
}

func (p *memProjection) SchemaDefs(typ string, scopes []string) []*Schema {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var defs []*Schema
	for _, scope := range scopes {
		for _, s := range p.schemas[scope] {
			if s.ArtifactType == typ {
				defs = append(defs, s)
			}
		}
	}
	return defs
}

func (p *memProjection) SchemasByScope() map[string][]*Schema {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := map[string][]*Schema{}
	for scope, defs := range p.schemas {
		out[scope] = append([]*Schema(nil), defs...)
	}
	return out
}

func (p *memProjection) Workflow(id string, scopes []string) *Workflow {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for i := len(scopes) - 1; i >= 0; i-- {
		if w, ok := p.workflows[scopes[i]][id]; ok {
			return w
		}
	}
	return nil
}

func (p *memProjection) Close() error { return nil }

// collect returns matching metas sorted by file path (callers hold p.mu).
func (p *memProjection) collect(filter func(*Meta) bool) []*Meta {
	var out []*Meta
	for _, m := range p.byGUID {
		if filter(m) {
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

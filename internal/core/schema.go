package core

import "strings"

// Schema is one schema definition file. The effective schema of an artifact
// is composed from all definitions with the same artifact type found while
// traversing the repository hierarchy from root to artifact (lexical
// inheritance, nearest definition wins).
type Schema struct {
	ArtifactType string         `json:"artifactType"`
	Kind         string         `json:"kind,omitempty"`
	DisplayName  string         `json:"displayName,omitempty"`
	Inheritance  string         `json:"inheritance,omitempty"` // "off" stops composition
	HIDPrefix    string         `json:"hidPrefix,omitempty"`
	Fields       []Field        `json:"fields,omitempty"`
	Workflows    []string       `json:"workflows,omitempty"`
	Relationships []Relationship `json:"relationships,omitempty"`
	Presentation map[string]any `json:"presentation,omitempty"`
}

type Field struct {
	ID       string   `json:"id"`
	Name     string   `json:"name,omitempty"`
	Type     string   `json:"type"`
	Multiple bool     `json:"multiple,omitempty"`
	Options  []string `json:"options,omitempty"`
}

type Relationship struct {
	LinkType    string   `json:"linkType"`
	SourceTypes []string `json:"sourceTypes,omitempty"` // empty = any
	TargetTypes []string `json:"targetTypes,omitempty"` // empty = any
	Cardinality string   `json:"cardinality,omitempty"`
}

type Workflow struct {
	ID          string       `json:"id"`
	Initial     string       `json:"initial"`
	States      []string     `json:"states"`
	Transitions []Transition `json:"transitions"`
}

type Transition struct {
	From string `json:"from"`
	To   string `json:"to"`
}

func (w *Workflow) CanTransition(from, to string) bool {
	for _, t := range w.Transitions {
		if t.From == from && t.To == to {
			return true
		}
	}
	return false
}

// scopeChain returns the configuration scopes from repository root down to
// folder, e.g. "a/b" -> ["", "a", "a/b"].
func scopeChain(folder string) []string {
	scopes := []string{""}
	if folder == "" {
		return scopes
	}
	segs := strings.Split(folder, "/")
	for i := range segs {
		scopes = append(scopes, strings.Join(segs[:i+1], "/"))
	}
	return scopes
}

// composeSchemas merges schema definitions ordered root -> artifact into the
// effective schema. Deeper definitions override: fields merge by id (replaced
// in place, new appended), scalar properties are overwritten when set,
// relationships merge by link type, workflows accumulate (deduplicated).
// A definition with inheritance "off" discards everything above it.
func composeSchemas(defs []*Schema) *Schema {
	start := 0
	for i, d := range defs {
		if strings.EqualFold(d.Inheritance, "off") {
			start = i
		}
	}
	eff := &Schema{Presentation: map[string]any{}}
	for _, d := range defs[start:] {
		eff.ArtifactType = d.ArtifactType
		if d.Kind != "" {
			eff.Kind = d.Kind
		}
		if d.DisplayName != "" {
			eff.DisplayName = d.DisplayName
		}
		if d.HIDPrefix != "" {
			eff.HIDPrefix = d.HIDPrefix
		}
		for _, f := range d.Fields {
			replaced := false
			for i, existing := range eff.Fields {
				if existing.ID == f.ID {
					eff.Fields[i] = f
					replaced = true
					break
				}
			}
			if !replaced {
				eff.Fields = append(eff.Fields, f)
			}
		}
		for _, r := range d.Relationships {
			replaced := false
			for i, existing := range eff.Relationships {
				if existing.LinkType == r.LinkType {
					eff.Relationships[i] = r
					replaced = true
					break
				}
			}
			if !replaced {
				eff.Relationships = append(eff.Relationships, r)
			}
		}
		for _, w := range d.Workflows {
			found := false
			for _, existing := range eff.Workflows {
				if existing == w {
					found = true
					break
				}
			}
			if !found {
				eff.Workflows = append(eff.Workflows, w)
			}
		}
		for k, v := range d.Presentation {
			eff.Presentation[k] = v
		}
	}
	if len(eff.Presentation) == 0 {
		eff.Presentation = nil
	}
	return eff
}

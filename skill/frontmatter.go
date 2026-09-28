package skill

import (
	"strconv"
	"strings"

	yaml "go.yaml.in/yaml/v3"
)

// ============================================================================
// Front matter: YAML semantics
//
// SKILL.md front matter used to be parsed line by line with the raw text after
// "key:" taken as the value, so `name: "12306"` registered a skill named
// `"12306"` (quotes included). use_skill 12306 could not find it and the
// enabled-state key skill."12306".enabled was rejected by the config store.
//
// The front matter is now decoded with a real YAML parser (go.yaml.in/yaml/v3,
// already in the module graph). Only top-level keys are read, scalars keep
// YAML semantics (single/double quotes with escapes, trailing comments,
// surrounding whitespace, block and multi-line plain scalars). Front matter
// that is not valid YAML (common in hand-written skills, e.g. an unquoted
// description containing ": ") falls back to the line parser, whose
// single-line values are decoded the same way field by field.
// ============================================================================

// ParseFrontMatter parses a SKILL.md document into its front matter and the
// Markdown body. Content without front matter is returned unchanged as body.
func ParseFrontMatter(content string) (SkillMeta, string) {
	return parseFrontMatter(content)
}

// decodeFrontMatterYAML decodes fm with the YAML parser. It returns false when
// fm is not valid YAML or not a mapping; meta is then left untouched.
func decodeFrontMatterYAML(fm string, meta *SkillMeta) bool {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(fm), &doc); err != nil {
		return false
	}
	if doc.Kind == 0 { // empty front matter
		return true
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return false
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return false
	}
	var out SkillMeta
	for i := 0; i+1 < len(root.Content); i += 2 {
		k, v := root.Content[i], root.Content[i+1]
		if k.Kind != yaml.ScalarNode {
			continue
		}
		if v.Kind == yaml.AliasNode && v.Alias != nil {
			v = v.Alias
		}
		key := strings.TrimSpace(k.Value)
		switch v.Kind {
		case yaml.ScalarNode:
			if key == "compatibility" {
				out.Compatibility = parseYAMLList(nodeScalar(v))
				continue
			}
			assignFrontMatterField(key, nodeScalar(v), &out)
		case yaml.SequenceNode:
			if key == "compatibility" {
				var list []string
				for _, item := range v.Content {
					if item.Kind == yaml.ScalarNode {
						if s := strings.TrimSpace(nodeScalar(item)); s != "" {
							list = append(list, s)
						}
					}
				}
				out.Compatibility = list
			}
		}
	}
	*meta = out
	return true
}

// nodeScalar returns the decoded value of a scalar node exactly as YAML
// defines it ("" for null); block scalars keep their chomping result.
func nodeScalar(n *yaml.Node) string {
	if n.Tag == "!!null" {
		return ""
	}
	return n.Value
}

// yamlScalar decodes a single-line YAML value (the text after "key:") with
// YAML semantics: quotes and escapes, trailing comments, whitespace, null.
// Values the YAML parser rejects (e.g. a plain value containing ": ") are
// decoded by plainScalarFallback.
func yamlScalar(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	var v struct {
		V yaml.Node `yaml:"v"`
	}
	if err := yaml.Unmarshal([]byte("v: "+raw), &v); err == nil && v.V.Kind == yaml.ScalarNode {
		return nodeScalar(&v.V)
	}
	return plainScalarFallback(raw)
}

// plainScalarFallback decodes a value that is not valid YAML on its own:
// a leading quoted string is unquoted (anything after its closing quote is
// ignored), otherwise a trailing " #comment" is removed.
func plainScalarFallback(raw string) string {
	switch {
	case strings.HasPrefix(raw, `"`):
		for i := 1; i < len(raw); i++ {
			if raw[i] == '\\' {
				i++
				continue
			}
			if raw[i] == '"' {
				if s, err := strconv.Unquote(raw[:i+1]); err == nil {
					return strings.TrimSpace(s)
				}
				return strings.TrimSpace(raw[1:i])
			}
		}
	case strings.HasPrefix(raw, `'`):
		for i := 1; i < len(raw); i++ {
			if raw[i] != '\'' {
				continue
			}
			if i+1 < len(raw) && raw[i+1] == '\'' {
				i++
				continue
			}
			return strings.TrimSpace(strings.ReplaceAll(raw[1:i], "''", "'"))
		}
	}
	return stripYAMLComment(raw)
}

// stripYAMLComment removes a trailing comment: '#' preceded by whitespace.
func stripYAMLComment(s string) string {
	for i := 1; i < len(s); i++ {
		if s[i] == '#' && (s[i-1] == ' ' || s[i-1] == '\t') {
			return strings.TrimSpace(s[:i])
		}
	}
	return strings.TrimSpace(s)
}

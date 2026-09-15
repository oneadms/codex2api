package proxy

import (
	"strconv"
	"strings"
)

// antigravityInlineLocalSchemaRefs expands local JSON Pointer $ref nodes against
// the schema root before union flattening and Gemini cleanup.
func antigravityInlineLocalSchemaRefs(root map[string]any) map[string]any {
	if root == nil {
		return nil
	}
	cloned := antigravityDeepCopySchemaValue(root)
	if clonedMap, ok := cloned.(map[string]any); ok {
		resolved := antigravityResolveLocalSchemaRefs(clonedMap, clonedMap, map[string]bool{})
		if out, ok := resolved.(map[string]any); ok {
			return out
		}
	}
	return root
}

func antigravityResolveLocalSchemaRefs(root, value any, active map[string]bool) any {
	switch typed := value.(type) {
	case map[string]any:
		if ref, ok := typed["$ref"].(string); ok {
			ref = strings.TrimSpace(ref)
			if strings.HasPrefix(ref, "#/") {
				if target, ok := antigravityResolveJSONSchemaPointer(root, ref); ok {
					if active[ref] {
						return antigravityCyclicSchemaRefFallback(typed, target, ref)
					}
					active[ref] = true
					resolvedTarget := antigravityResolveLocalSchemaRefs(root, antigravityDeepCopySchemaValue(target), active)
					delete(active, ref)
					merged, ok := resolvedTarget.(map[string]any)
					if !ok {
						return antigravityUnresolvedSchemaRefFallback(typed, ref)
					}
					out := make(map[string]any, len(merged)+len(typed))
					for key, item := range merged {
						out[key] = item
					}
					for key, item := range typed {
						if key == "$ref" {
							continue
						}
						out[key] = antigravityResolveLocalSchemaRefs(root, item, active)
					}
					return out
				}
				return antigravityUnresolvedSchemaRefFallback(typed, ref)
			}
		}
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			out[key] = antigravityResolveLocalSchemaRefs(root, item, active)
		}
		return out
	case []any:
		out := make([]any, 0, len(typed))
		for _, item := range typed {
			out = append(out, antigravityResolveLocalSchemaRefs(root, item, active))
		}
		return out
	default:
		return typed
	}
}

func antigravityResolveJSONSchemaPointer(root any, ref string) (any, bool) {
	current := root
	for _, rawPart := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		if rawPart == "" {
			continue
		}
		part := strings.ReplaceAll(strings.ReplaceAll(rawPart, "~1", "/"), "~0", "~")
		switch node := current.(type) {
		case map[string]any:
			next, ok := node[part]
			if !ok {
				return nil, false
			}
			current = next
		case []any:
			index, err := strconv.Atoi(part)
			if err != nil || index < 0 || index >= len(node) {
				return nil, false
			}
			current = node[index]
		default:
			return nil, false
		}
	}
	return current, true
}

func antigravityCyclicSchemaRefFallback(node map[string]any, target any, ref string) map[string]any {
	out := make(map[string]any)
	if targetMap, ok := target.(map[string]any); ok {
		for _, key := range []string{"type", "nullable", "description"} {
			if value, exists := targetMap[key]; exists {
				out[key] = value
			}
		}
	}
	for key, value := range node {
		if key != "$ref" {
			out[key] = value
		}
	}
	hint := "See: " + antigravitySchemaRefName(ref)
	out["description"] = antigravityMergeSchemaHint(asString(out["description"]), hint)
	return out
}

func antigravityUnresolvedSchemaRefFallback(node map[string]any, ref string) map[string]any {
	out := map[string]any{"type": "object"}
	hint := "See: " + antigravitySchemaRefName(ref)
	if description, ok := node["description"].(string); ok && strings.TrimSpace(description) != "" {
		out["description"] = antigravityMergeSchemaHint(description, hint)
	} else {
		out["description"] = hint
	}
	for key, value := range node {
		if key == "$ref" {
			continue
		}
		if key == "description" {
			continue
		}
		out[key] = value
	}
	return out
}

func antigravitySchemaRefName(ref string) string {
	ref = strings.TrimPrefix(ref, "#/")
	if index := strings.LastIndex(ref, "/"); index >= 0 && index+1 < len(ref) {
		name := ref[index+1:]
		return strings.ReplaceAll(strings.ReplaceAll(name, "~1", "/"), "~0", "~")
	}
	return ref
}

func antigravityDeepCopySchemaValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			out[key] = antigravityDeepCopySchemaValue(item)
		}
		return out
	case []any:
		out := make([]any, 0, len(typed))
		for _, item := range typed {
			out = append(out, antigravityDeepCopySchemaValue(item))
		}
		return out
	default:
		return typed
	}
}

func antigravitySchemaHasRef(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		if ref, ok := typed["$ref"].(string); ok && strings.HasPrefix(strings.TrimSpace(ref), "#/") {
			return true
		}
		for _, item := range typed {
			if antigravitySchemaHasRef(item) {
				return true
			}
		}
	case []any:
		for _, item := range typed {
			if antigravitySchemaHasRef(item) {
				return true
			}
		}
	}
	return false
}

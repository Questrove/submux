package override

import (
	"strings"

	"gopkg.in/yaml.v3"
)

// Apply 把 Merge YAML 覆盖应用到主模型,返回新模型(不修改入参)。
func Apply(cfg map[string]any, overrideYAML string) (map[string]any, error) {
	if strings.TrimSpace(overrideYAML) == "" {
		return cfg, nil
	}
	var ov map[string]any
	if err := yaml.Unmarshal([]byte(overrideYAML), &ov); err != nil {
		return nil, err
	}
	return deepMerge(cfg, ov), nil
}

func deepMerge(base, ov map[string]any) map[string]any {
	out := make(map[string]any, len(base))
	for k, v := range base {
		out[k] = v
	}
	// 第一遍:普通字段(深合并 / 覆盖)
	for k, v := range ov {
		if isDirective(k) {
			continue
		}
		if bm, ok := asMap(out[k]); ok {
			if vm, ok2 := asMap(v); ok2 {
				out[k] = deepMerge(bm, vm)
				continue
			}
		}
		out[k] = v
	}
	// 第二遍:prepend-/append- 指令
	for k, v := range ov {
		if !isDirective(k) {
			continue
		}
		op, target := splitDirective(k)
		items := asSlice(v)
		existing := asSlice(out[target])
		if op == "prepend" {
			out[target] = concat(items, existing)
		} else {
			out[target] = concat(existing, items)
		}
	}
	return out
}

func isDirective(k string) bool {
	return strings.HasPrefix(k, "prepend-") || strings.HasPrefix(k, "append-")
}

func splitDirective(k string) (op, target string) {
	if strings.HasPrefix(k, "prepend-") {
		return "prepend", strings.TrimPrefix(k, "prepend-")
	}
	return "append", strings.TrimPrefix(k, "append-")
}

func asMap(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

func asSlice(v any) []any {
	if s, ok := v.([]any); ok {
		return s
	}
	return nil
}

func concat(a, b []any) []any {
	out := make([]any, 0, len(a)+len(b))
	out = append(out, a...)
	out = append(out, b...)
	return out
}

package plugin

import (
	"fmt"
	"sort"
)

// findCapSpec 在插件清单里找某个能力的详细声明。
func findCapSpec(man *Manifest, name string) *CapabilitySpec {
	for i := range man.Capabilities {
		if man.Capabilities[i].Name == name || capName(man.Capabilities[i].Name) == name {
			return &man.Capabilities[i]
		}
	}
	return nil
}

// validateArgs 用能力声明的 schema 校验参数。
// 支持 JSON Schema 的最小子集：type / properties / required。
// 没声明 schema 的能力不做任何检查（保持兼容）。
func validateArgs(schema map[string]any, args map[string]any) error {
	if len(schema) == 0 {
		return nil
	}

	props, _ := schema["properties"].(map[string]any)

	// 必填项
	if required, ok := schema["required"].([]any); ok {
		for _, r := range required {
			name, _ := r.(string)
			if name == "" {
				continue
			}
			if _, exists := args[name]; !exists {
				return fmt.Errorf("缺少必填参数 %q", name)
			}
		}
	}

	// 已知参数类型
	for k, v := range args {
		raw, ok := props[k]
		if !ok {
			if len(props) > 0 {
				return fmt.Errorf("未知参数 %q（允许：%v）", k, sortedKeys(props))
			}
			continue
		}
		ps, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := ps["type"].(string)
		if typ != "" && !matchType(typ, v) {
			return fmt.Errorf("参数 %q 应为 %s，实际传了 %T", k, typ, v)
		}
	}
	return nil
}

func matchType(typ string, v any) bool {
	switch typ {
	case "string":
		_, ok := v.(string)
		return ok
	case "number", "integer":
		_, ok := v.(float64)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "object":
		_, ok := v.(map[string]any)
		return ok
	}
	return true // 不认识的类型不拦
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

package plugin

import (
	"context"
	"database/sql"
	"fmt"
)

// BuiltinCapability 是框架自己提供的能力（不需要插件）。
// 典型场景：框架已经内置了 SQLite，就没必要再让插件绕一圈 RPC。
type BuiltinCapability func(ctx context.Context, args map[string]any) (any, error)

// registerBuiltins 注册框架内置能力。main 里拿到 storage 之后调用。
func (m *Manager) RegisterBuiltins(db *sql.DB) {
	if m.Builtins == nil {
		m.Builtins = make(map[string]BuiltinCapability)
	}
	if db == nil {
		return
	}

	m.Builtins["db.sqlite"] = func(ctx context.Context, args map[string]any) (any, error) {
		return runSQLiteQuery(ctx, db, args)
	}
}

// runSQLiteQuery 执行一条 SQL。
// op: exec | all（默认 all）| get（取第一行）
func runSQLiteQuery(ctx context.Context, db *sql.DB, args map[string]any) (any, error) {
	sqlText, _ := args["sql"].(string)
	if sqlText == "" {
		return nil, fmt.Errorf("缺少 sql")
	}
	op, _ := args["op"].(string)
	if op == "" {
		op = "all"
	}

	var params []any
	if raw, ok := args["params"].([]any); ok {
		params = raw
	}

	switch op {
	case "exec":
		res, err := db.ExecContext(ctx, sqlText, params...)
		if err != nil {
			return nil, err
		}
		affected, _ := res.RowsAffected()
		lastID, _ := res.LastInsertId()
		return map[string]any{"affected": affected, "last_id": lastID}, nil


	case "all", "":
		rows, err := db.QueryContext(ctx, sqlText, params...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		return scanAllRows(rows)

	case "get":
		// 取第一行：查不到返回 {"row": null}，别报错。
		// 插件里最常见的写法就是"按主键查一条"，用 all 再取 [0] 太啰嗦。
		rows, err := db.QueryContext(ctx, sqlText, params...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		res, err := scanAllRows(rows)
		if err != nil {
			return nil, err
		}
		m := res.(map[string]any)
		list, _ := m["rows"].([]map[string]any)
		if len(list) == 0 {
			return map[string]any{"row": nil}, nil
		}
		return map[string]any{"row": list[0]}, nil

	default:
		return nil, fmt.Errorf("未知 op %q（支持 exec / all / get）", op)
	}
}

// scanAllRows 把结果集扫成 []map。列值尽量还原成 JSON 友好类型。
func scanAllRows(rows *sql.Rows) (any, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, 8)
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := make(map[string]any, len(cols))
		for i, c := range cols {
			row[c] = normalizeValue(vals[i])
		}
		out = append(out, row)
	}
	return map[string]any{"rows": out, "count": len(out)}, rows.Err()
}

// normalizeValue 把 []byte 转成字符串（SQLite 的 TEXT 返回 []byte）。
func normalizeValue(v any) any {
	switch x := v.(type) {
	case []byte:
		return string(x)
	default:
		return v
	}
}

// BuiltinNames 返回框架内置能力的名字，给 WebUI 展示用。
func (m *Manager) BuiltinNames() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, 0, len(m.Builtins))
	for name := range m.Builtins {
		out = append(out, name)
	}
	return out
}

package plugin

import (
	"bytes"
	"encoding/json"
	"fmt"
)

const JSONRPCVersion = "2.0"

// JSON-RPC 标准错误码 + 本协议自定义段（-32000 起）。
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603

	CodePermissionDenied = -32000
	CodeTimeout          = -32001
	CodeUnavailable      = -32002
	CodeConflict         = -32003
	CodeVersionMismatch  = -32004
	CodeRateLimited      = -32005
)

// Request 是 JSON-RPC 请求帧（双向共用）。
// ID 用 RawMessage：JSON-RPC 2.0 允许字符串和数字两种 id，
// 宿主内部统一用字符串关联，回包时按原样回显。
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	// Plugin 是共享宿主（cohost）模式的多路复用标：独占进程不会出现该字段。
	// M1 阶段宿主只容忍不路由；M2 的 SharedHost 按 (Plugin, ID) 复合键路由。
	Plugin string          `json:"plugin,omitempty"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// Response 是 JSON-RPC 响应帧（双向共用）。
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	// Plugin 同 Request：共享宿主模式的来源标（独占进程不出现）。
	Plugin string          `json:"plugin,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *RPCError       `json:"error,omitempty"`
}

// RPCStringID 把内部字符串 id 编码成 JSON id（带引号的字符串字面量）。
func RPCStringID(id string) json.RawMessage {
	b, err := json.Marshal(id)
	if err != nil {
		return json.RawMessage(`""`)
	}
	return b
}

// RPCIDKey 把线上的 id 字面量归一成关联用的字符串 key：
// 字符串 id 去引号，数字等其他类型用字面量本身。
func RPCIDKey(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return string(bytes.TrimSpace(raw))
}

// RPCError 是错误对象。
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *RPCError) Error() string { return e.Message }

// Errf 构造一个错误响应。
func Errf(code int, format string, args ...any) *RPCError {
	msg := format
	if len(args) > 0 {
		msg = fmt.Sprintf(format, args...)
	}
	return &RPCError{Code: code, Message: msg}
}

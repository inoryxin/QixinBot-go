package plugin

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// 帧的编解码是协议层最容易出错的地方：字段名写错、omitempty 用错、
// 错误码对不上，都会让插件和宿主鸡同鸭讲。

func TestRequestMarshalOmitsEmptyID(t *testing.T) {
	// 通知帧（没有 id）不能出现 "id":""，否则对方会当请求去响应
	raw, err := json.Marshal(Request{JSONRPC: "2.0", Method: "notify"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"id"`) {
		t.Fatalf("通知帧不该带 id 字段: %s", raw)
	}
}

func TestRequestRoundTrip(t *testing.T) {
	in := Request{
		JSONRPC: "2.0",
		ID:      RPCStringID("42"),
		Method:  "plugin.handle_message",
		Params:  json.RawMessage(`{"session":{"text":"hi"}}`),
	}
	raw, _ := json.Marshal(in)

	var out Request
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if RPCIDKey(out.ID) != "42" || out.Method != in.Method {
		t.Fatalf("往返后字段丢了: %+v", out)
	}
	if string(out.Params) != string(in.Params) {
		t.Fatalf("params 变了: %s -> %s", in.Params, out.Params)
	}
}

func TestNumericIDRoundTrip(t *testing.T) {
	// JSON-RPC 2.0 允许数字 id；不用官方 SDK 的插件可能发数字。
	// 之前 ID 用 string 接，数字解不出来，整帧被当 stdout 污染丢弃。
	raw := []byte(`{"jsonrpc":"2.0","id":7,"method":"host.log"}`)
	var req Request
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatalf("数字 id 解析失败: %v", err)
	}
	if req.Method != "host.log" {
		t.Fatalf("method 丢了: %q", req.Method)
	}
	if RPCIDKey(req.ID) != "7" {
		t.Fatalf("数字 id 归一错误: %q", string(req.ID))
	}

	// 回包按原样回显
	resp := Response{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(`{}`)}
	out, _ := json.Marshal(resp)
	if !strings.Contains(string(out), `"id":7`) {
		t.Fatalf("数字 id 没有原样回显: %s", out)
	}
}

func TestResponseWithResult(t *testing.T) {
	raw := []byte(`{"jsonrpc":"2.0","id":"1","result":{"ok":true}}`)
	var resp Response
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error != nil {
		t.Fatal("成功响应不该有 error")
	}
	var got map[string]any
	_ = json.Unmarshal(resp.Result, &got)
	if got["ok"] != true {
		t.Fatalf("result 解析错了: %v", got)
	}
}

func TestResponseWithError(t *testing.T) {
	raw := []byte(`{"jsonrpc":"2.0","id":"1","error":{"code":-32601,"message":"未实现"}}`)
	var resp Response
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error == nil {
		t.Fatal("应该带 error")
	}
	if resp.Error.Code != CodeMethodNotFound {
		t.Fatalf("错误码应该是 %d，实际 %d", CodeMethodNotFound, resp.Error.Code)
	}
	if resp.Error.Error() != "未实现" {
		t.Fatalf("Error() 应该返回 message，实际 %q", resp.Error.Error())
	}
}

func TestErrorCodesAreDistinct(t *testing.T) {
	// 这些码是插件用来判断"要不要重试"的依据，不能撞车
	codes := map[int]string{
		CodeParseError:       "ParseError",
		CodeInvalidRequest:   "InvalidRequest",
		CodeMethodNotFound:   "MethodNotFound",
		CodeInvalidParams:    "InvalidParams",
		CodeInternalError:    "InternalError",
		CodePermissionDenied: "PermissionDenied",
		CodeTimeout:          "Timeout",
		CodeUnavailable:      "Unavailable",
		CodeConflict:         "Conflict",
		CodeVersionMismatch:  "VersionMismatch",
		CodeRateLimited:      "RateLimited",
	}
	if len(codes) != 11 {
		t.Fatalf("错误码有重复，只剩 %d 个独立值", len(codes))
	}
}

func TestErrfBuildsError(t *testing.T) {
	err := Errf(CodeTimeout, "调用 %s 超时", "llm.chat")
	if err == nil {
		t.Fatal("应该返回错误")
	}
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("应该是 *RPCError，实际 %T", err)
	}
	if rpcErr.Code != CodeTimeout {
		t.Fatalf("错误码不对: %d", rpcErr.Code)
	}
	if rpcErr.Message != "调用 llm.chat 超时" {
		t.Fatalf("消息格式化不对: %q", rpcErr.Message)
	}
}

func TestParseErrorIsRPCError(t *testing.T) {
	// 插件 SDK 靠 code 区分"参数错了"和"宿主暂时不可用"
	err := Errf(CodeInvalidParams, "缺少 name")
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != CodeInvalidParams {
		t.Fatalf("解析失败: %v", err)
	}
}

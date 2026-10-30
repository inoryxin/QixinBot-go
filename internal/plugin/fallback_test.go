package plugin

import (
	"errors"
	"testing"
)

// 能力降级的边界：该重试的重试，不该重试的立刻返回。

func TestRetryableCapErrTimeout(t *testing.T) {
	if !retryableCapErr(Errf(CodeTimeout, "超时")) {
		t.Fatal("超时应该换下一个提供者")
	}
}

func TestRetryableCapErrUnavailable(t *testing.T) {
	if !retryableCapErr(Errf(CodeUnavailable, "服务没起来")) {
		t.Fatal("不可用应该换下一个")
	}
}

func TestRetryableCapErrInternal(t *testing.T) {
	if !retryableCapErr(Errf(CodeInternalError, "崩了")) {
		t.Fatal("内部错误应该换下一个")
	}
}

func TestNotRetryableInvalidParams(t *testing.T) {
	// 参数错换谁都一样，重试只是浪费 15 秒
	if retryableCapErr(Errf(CodeInvalidParams, "缺 city")) {
		t.Fatal("参数错不该重试")
	}
}

func TestNotRetryablePermission(t *testing.T) {
	if retryableCapErr(Errf(CodePermissionDenied, "没权限")) {
		t.Fatal("权限错不该重试")
	}
}

func TestRetryableNonRPCError(t *testing.T) {
	// 进程直接挂了（不是 RPC 错误）也值得试下一个
	if !retryableCapErr(errors.New("read: file already closed")) {
		t.Fatal("非 RPC 错误也应该重试")
	}
}

func TestRetryableNilIsFalse(t *testing.T) {
	// nil 不该被当成可重试（虽然正常不会传进来）
	if retryableCapErr(nil) {
		t.Fatal("nil 不该算可重试")
	}
}

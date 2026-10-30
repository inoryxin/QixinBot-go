package webui

import (
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func testServer() *Server {
	return &Server{
		Password: "p",
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		sessions: map[string]time.Time{"tok123": time.Now().Add(time.Hour)},
		sessFile: "test-sessions.json",
	}
}

func TestGuardRejectsCSRF(t *testing.T) {
	s := testServer()
	h := s.guard(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	do := func(token, origin string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/tasks/pause", strings.NewReader("_csrf="+token))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(&http.Cookie{Name: cookieName, Value: "tok123"})
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		h(w, req)
		return w.Code
	}

	if code := do("", ""); code != http.StatusForbidden {
		t.Fatalf("无 token 应 403，got %d", code)
	}
	if code := do("wrong", ""); code != http.StatusForbidden {
		t.Fatalf("错误 token 应 403，got %d", code)
	}
	if code := do("tok123", "https://evil.example.com"); code != http.StatusForbidden {
		t.Fatalf("跨站 Origin 应 403，got %d", code)
	}
	if code := do("tok123", ""); code != http.StatusOK {
		t.Fatalf("正确 token 应 200，got %d", code)
	}
}

func TestBase64ImageRejectsHTML(t *testing.T) {
	html := base64.StdEncoding.EncodeToString([]byte("<html><script>alert(1)</script></html>"))
	req := httptest.NewRequest(http.MethodGet, "/api/chat/image?url="+url.QueryEscape("base64://"+html), nil)
	w := httptest.NewRecorder()
	(&Server{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}).apiChatImage(w, req)
	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("HTML base64 应被拒绝，got %d", w.Code)
	}
}

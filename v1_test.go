package m

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// =============================================================================
// Body size limit
// =============================================================================

func TestBodySizeLimit_DefaultEnforced(t *testing.T) {
	Reset()
	defer Reset()

	type Body struct {
		Data string `json:"data"`
	}

	handler := H(func(b JSON[Body]) string { return "ok" })

	// Build a body larger than DefaultMaxRequestBodySize.
	big := make([]byte, DefaultMaxRequestBodySize+1024)
	for i := range big {
		big[i] = 'a'
	}
	payload, _ := json.Marshal(Body{Data: string(big)})

	req := httptest.NewRequest("POST", "/", bytes.NewReader(payload))
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d (body=%s)", rec.Code, rec.Body.String())
	}

	var resp HTTPError
	parseJSONResponse(t, rec.Body.Bytes(), &resp)
	if resp.Err != "body_too_large" {
		t.Errorf("expected error=body_too_large, got %s", resp.Err)
	}
}

func TestBodySizeLimit_Configurable(t *testing.T) {
	Reset()
	defer Reset()

	Configure(WithMaxRequestBodySize(32)) // 32 bytes

	type Body struct {
		Data string `json:"data"`
	}
	handler := H(func(b JSON[Body]) string { return b.Value.Data })

	payload := []byte(`{"data":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}`)
	if len(payload) <= 32 {
		t.Fatalf("test payload should be > 32 bytes")
	}

	req := httptest.NewRequest("POST", "/", bytes.NewReader(payload))
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("expected 413 with small limit, got %d", rec.Code)
	}
}

func TestBodySizeLimit_DisabledWhenZero(t *testing.T) {
	Reset()
	defer Reset()
	Configure(WithMaxRequestBodySize(0))

	type Body struct {
		Data string `json:"data"`
	}
	handler := H(func(b JSON[Body]) string { return "ok" })

	payload, _ := json.Marshal(Body{Data: strings.Repeat("x", 10_000_000)}) // 10MB

	req := httptest.NewRequest("POST", "/", bytes.NewReader(payload))
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 when limit disabled, got %d", rec.Code)
	}
}

func TestBodySizeLimit_FormEnforced(t *testing.T) {
	Reset()
	defer Reset()
	Configure(WithMaxRequestBodySize(16))

	type F struct {
		Username string `schema:"username"`
	}
	handler := H(func(f Form[F]) string { return f.Value.Username })

	payload := strings.NewReader("username=" + strings.Repeat("a", 200))
	req := httptest.NewRequest("POST", "/", payload)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("expected 413, got %d", rec.Code)
	}
}

// =============================================================================
// Default JSON encoding does NOT escape <, >, &
// =============================================================================

func TestDefaultJSON_DoesNotEscapeHTML(t *testing.T) {
	Reset()
	defer Reset()

	handler := H(func() map[string]string {
		return map[string]string{"v": "<a>&</a>"}
	})

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "<a>&</a>") {
		t.Errorf("expected raw HTML chars in JSON, got %q", body)
	}
	if strings.Contains(body, `\u003c`) || strings.Contains(body, `\u0026`) {
		t.Errorf("did not expect HTML escapes, got %q", body)
	}
}

func TestDefaultJSON_NoTrailingNewline(t *testing.T) {
	Reset()
	defer Reset()

	handler := H(func() map[string]string {
		return map[string]string{"k": "v"}
	})

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	body := rec.Body.Bytes()
	if len(body) == 0 || body[len(body)-1] == '\n' {
		t.Errorf("expected no trailing newline, got %q", string(body))
	}
}

// =============================================================================
// 4xx message preserved; 5xx message hidden
// =============================================================================

func TestErrorMessage_4xxPreserved(t *testing.T) {
	Reset()
	defer Reset()

	cases := []struct {
		err            error
		expectCode     int
		expectMessage  string
		messageVisible bool
	}{
		{errors.New("user 123 not found"), 404, "user 123 not found", true},
		{errors.New("invalid token shape"), 400, "invalid token shape", true},
		{errors.New("unauthorized: bad creds"), 401, "unauthorized: bad creds", true},
		{errors.New("forbidden: needs admin"), 403, "forbidden: needs admin", true},
		{errors.New("request timeout"), 408, "request timeout", true},
		{errors.New("database exploded"), 500, "", false},
		{errors.New("something went wrong"), 500, "", false},
	}

	for _, tc := range cases {
		t.Run(tc.err.Error(), func(t *testing.T) {
			handler := H(func() error { return tc.err })
			req := httptest.NewRequest("GET", "/", nil)
			rec := httptest.NewRecorder()
			handler(rec, req)

			if rec.Code != tc.expectCode {
				t.Fatalf("code: got %d, want %d", rec.Code, tc.expectCode)
			}

			var resp HTTPError
			parseJSONResponse(t, rec.Body.Bytes(), &resp)

			if tc.messageVisible {
				if resp.Message != tc.expectMessage {
					t.Errorf("message: got %q, want %q", resp.Message, tc.expectMessage)
				}
			} else {
				if resp.Message != "" {
					t.Errorf("5xx must hide message, got %q", resp.Message)
				}
			}
		})
	}
}

func TestErrorMessage_5xxLoggedNotLeaked(t *testing.T) {
	Reset()
	defer Reset()

	var buf bytes.Buffer
	Configure(WithLogger(log.New(&buf, "", 0)))

	handler := H(func() error { return errors.New("secret DB url=postgres://x") })

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != 500 {
		t.Fatalf("expected 500, got %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "secret DB") {
		t.Errorf("5xx body must not contain original message; got %s", rec.Body.String())
	}
	if !strings.Contains(buf.String(), "secret DB") {
		t.Errorf("expected original 5xx error to be logged; log=%q", buf.String())
	}
}

// =============================================================================
// ResponseWriter: Flusher / Hijacker / Pusher
// =============================================================================

// flushRecorder is httptest.ResponseRecorder + a Flush method (which exists
// already), exercised via the wrapper.
func TestResponseWriter_Flush(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := &ResponseWriter{ResponseWriter: rec}
	// Should be safe and a no-op if underlying supports Flusher (httptest
	// recorder does implement Flush() actually). Calling must not panic.
	rw.Flush()
}

type plainRW struct{ http.ResponseWriter }

func TestResponseWriter_HijackNotSupported(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := &ResponseWriter{ResponseWriter: plainRW{rec}}

	_, _, err := rw.Hijack()
	if !errors.Is(err, http.ErrNotSupported) {
		t.Errorf("expected ErrNotSupported, got %v", err)
	}
}

func TestResponseWriter_PushNotSupported(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := &ResponseWriter{ResponseWriter: plainRW{rec}}
	err := rw.Push("/foo", nil)
	if !errors.Is(err, http.ErrNotSupported) {
		t.Errorf("expected ErrNotSupported, got %v", err)
	}
}

// hijackable simulates an http.Hijacker.
type hijackable struct {
	http.ResponseWriter
	called bool
}

func (h *hijackable) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h.called = true
	return nil, nil, nil
}

func TestResponseWriter_HijackDelegates(t *testing.T) {
	hj := &hijackable{ResponseWriter: httptest.NewRecorder()}
	rw := &ResponseWriter{ResponseWriter: hj}
	_, _, _ = rw.Hijack()
	if !hj.called {
		t.Error("expected underlying Hijack to be called")
	}
}

// =============================================================================
// Initialize: warning on second call, auto-create validator
// =============================================================================

func TestInitialize_WarnsOnRepeat(t *testing.T) {
	Reset()
	defer Reset()

	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)

	Initialize(WithLogger(logger))
	Initialize(WithLogger(logger)) // should warn

	if !strings.Contains(buf.String(), "Initialize called more than once") {
		t.Errorf("expected warning, log=%q", buf.String())
	}
}

func TestInitialize_AutoCreatesValidator(t *testing.T) {
	Reset()
	defer Reset()

	// Validator nil, EnableValidation true → should be auto-created.
	Initialize(WithValidation(true))

	type Body struct {
		Email string `json:"email" validate:"required,email"`
	}
	handler := H(func(b JSON[Body]) string { return "ok" })

	req := httptest.NewRequest("POST", "/", strings.NewReader(`{"email":"bad"}`))
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != 400 {
		t.Errorf("expected validation failure 400, got %d", rec.Code)
	}
}

// =============================================================================
// Header[T] extractor
// =============================================================================

type headerStruct struct {
	RequestID string `header:"X-Request-ID"`
	Age       int    `header:"X-Age"`
	Admin     bool   `header:"X-Admin"`
	Skipped   string `header:"-"`
}

func TestHeaderExtractor(t *testing.T) {
	Reset()
	defer Reset()

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Request-ID", "abc-123")
	req.Header.Set("X-Age", "42")
	req.Header.Set("X-Admin", "true")
	req.Header.Set("-", "should not bind")

	var h Header[headerStruct]
	if err := h.Extract(req); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if h.Value.RequestID != "abc-123" {
		t.Errorf("RequestID: got %q", h.Value.RequestID)
	}
	if h.Value.Age != 42 {
		t.Errorf("Age: got %d", h.Value.Age)
	}
	if !h.Value.Admin {
		t.Errorf("Admin: got %v", h.Value.Admin)
	}
	if h.Value.Skipped != "" {
		t.Errorf("skipped field set: %q", h.Value.Skipped)
	}
}

func TestHeaderExtractor_MissingIsZero(t *testing.T) {
	Reset()
	defer Reset()

	req := httptest.NewRequest("GET", "/", nil)
	var h Header[headerStruct]
	if err := h.Extract(req); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if h.Value.RequestID != "" || h.Value.Age != 0 || h.Value.Admin {
		t.Errorf("missing headers should yield zero values, got %+v", h.Value)
	}
}

func TestHeaderExtractor_InvalidConversion(t *testing.T) {
	Reset()
	defer Reset()

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Age", "not-a-number")

	var h Header[headerStruct]
	err := h.Extract(req)
	if err == nil {
		t.Fatal("expected error for invalid int header")
	}
	var ee *ExtractError
	if !errors.As(err, &ee) || ee.Type != "header_conversion_error" {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestHeaderExtractor_InH(t *testing.T) {
	Reset()
	defer Reset()

	handler := H(func(h Header[headerStruct]) string {
		return fmt.Sprintf("%s/%d", h.Value.RequestID, h.Value.Age)
	})

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Request-ID", "x1")
	req.Header.Set("X-Age", "9")
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Body.String() != "x1/9" {
		t.Errorf("got %q", rec.Body.String())
	}
}

func TestHeaderExtractor_NonStructTypePanicsAtUse(t *testing.T) {
	Reset()
	defer Reset()

	// Extract on a non-struct T should return an error, not panic at compile.
	var h Header[string]
	req := httptest.NewRequest("GET", "/", nil)
	err := h.Extract(req)
	if err == nil {
		t.Fatal("expected error for non-struct T")
	}
}

// =============================================================================
// Cookie[T] extractor
// =============================================================================

type cookieStruct struct {
	Session string `cookie:"session_id"`
	Theme   string `cookie:"theme"`
	Count   int    `cookie:"count"`
}

func TestCookieExtractor(t *testing.T) {
	Reset()
	defer Reset()

	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: "session_id", Value: "sess-123"})
	req.AddCookie(&http.Cookie{Name: "theme", Value: "dark"})
	req.AddCookie(&http.Cookie{Name: "count", Value: "7"})

	var c Cookie[cookieStruct]
	if err := c.Extract(req); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if c.Value.Session != "sess-123" || c.Value.Theme != "dark" || c.Value.Count != 7 {
		t.Errorf("got %+v", c.Value)
	}
}

func TestCookieExtractor_MissingIsZero(t *testing.T) {
	Reset()
	defer Reset()

	req := httptest.NewRequest("GET", "/", nil)
	var c Cookie[cookieStruct]
	if err := c.Extract(req); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if c.Value.Session != "" || c.Value.Count != 0 {
		t.Errorf("missing cookies should be zero: %+v", c.Value)
	}
}

func TestCookieExtractor_InvalidConversion(t *testing.T) {
	Reset()
	defer Reset()

	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: "count", Value: "x"})
	var c Cookie[cookieStruct]
	err := c.Extract(req)
	if err == nil {
		t.Fatal("expected error for invalid int cookie")
	}
}

// =============================================================================
// Path[T] mismatch handling
// =============================================================================

func TestPathMismatch_ReturnsBadRequest(t *testing.T) {
	Reset()
	defer Reset()

	// Handler has 2 Path[T] params but pattern has only 1.
	handler := H(func(a Path[int], b Path[int]) int { return a.Value + b.Value })

	req := createRequestWithPattern("GET", "/x/1", "/x/{a}")
	req.SetPathValue("a", "1")
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != 400 {
		t.Errorf("expected 400, got %d", rec.Code)
	}
}

// =============================================================================
// Concurrent body limit + path cache safety
// =============================================================================

func TestPatternCache_Concurrent(t *testing.T) {
	Reset()
	defer Reset()

	handler := H(func(id Path[int]) int { return id.Value })

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			req := createRequestWithPattern("GET", fmt.Sprintf("/items/%d", n), "/items/{id}")
			req.SetPathValue("id", fmt.Sprintf("%d", n))
			rec := httptest.NewRecorder()
			handler(rec, req)
			if rec.Code != 200 {
				t.Errorf("expected 200, got %d", rec.Code)
			}
		}(i)
	}
	wg.Wait()
}

// =============================================================================
// Existing semantics safeguards (regression tests for the rewrite)
// =============================================================================

func TestPath_KeyAccessor(t *testing.T) {
	var p Path[int]
	p.SetKey("foo")
	if p.Key() != "foo" {
		t.Errorf("Key(): got %q want foo", p.Key())
	}
}

func TestHTTPError_413WithMaxBytesError(t *testing.T) {
	err := &http.MaxBytesError{Limit: 100}
	hr := toHTTPError(err)
	if hr.Code != 413 {
		t.Errorf("expected 413, got %d", hr.Code)
	}
}

// _ ensures io is used (for future extension tests).
var _ = io.Discard

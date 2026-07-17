package m

// Regression tests for the v1.2.0 bugfix round. Each test names the bug it
// guards against; see CHANGELOG "v1.2.0".

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// =============================================================================
// B1: JSON[T] must accept non-struct bodies (slice, map, scalar, time.Time)
// =============================================================================

func TestJSON_NonStructBody(t *testing.T) {
	Reset()
	defer Reset()

	t.Run("slice of strings", func(t *testing.T) {
		handler := H(func(b JSON[[]string]) string { return strings.Join(b.Value, ",") })
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest("POST", "/", strings.NewReader(`["a","b"]`)))
		if rec.Code != 200 {
			t.Fatalf("expected 200, got %d (body=%s)", rec.Code, rec.Body.String())
		}
		if rec.Body.String() != "a,b" {
			t.Errorf("unexpected body: %s", rec.Body.String())
		}
	})

	t.Run("slice of structs", func(t *testing.T) {
		handler := H(func(b JSON[[]User]) string { return fmt.Sprint(len(b.Value)) })
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest("POST", "/", strings.NewReader(`[{"name":"a"},{"name":"b"}]`)))
		if rec.Code != 200 {
			t.Fatalf("expected 200, got %d (body=%s)", rec.Code, rec.Body.String())
		}
		if rec.Body.String() != "2" {
			t.Errorf("unexpected body: %s", rec.Body.String())
		}
	})

	t.Run("map body", func(t *testing.T) {
		handler := H(func(b JSON[map[string]int]) string { return fmt.Sprint(b.Value["a"]) })
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest("POST", "/", strings.NewReader(`{"a":3}`)))
		if rec.Code != 200 {
			t.Fatalf("expected 200, got %d (body=%s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("scalar body", func(t *testing.T) {
		handler := H(func(b JSON[int]) string { return fmt.Sprint(b.Value) })
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest("POST", "/", strings.NewReader(`42`)))
		if rec.Code != 200 {
			t.Fatalf("expected 200, got %d (body=%s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("time.Time body", func(t *testing.T) {
		handler := H(func(b JSON[time.Time]) string { return fmt.Sprint(b.Value.Year()) })
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest("POST", "/", strings.NewReader(`"2026-07-17T10:00:00Z"`)))
		if rec.Code != 200 {
			t.Fatalf("expected 200, got %d (body=%s)", rec.Code, rec.Body.String())
		}
		if rec.Body.String() != "2026" {
			t.Errorf("unexpected body: %s", rec.Body.String())
		}
	})

	t.Run("struct validation still enforced", func(t *testing.T) {
		type reqBody struct {
			Name string `json:"name" validate:"required"`
		}
		handler := H(func(b JSON[reqBody]) string { return "ok" })
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest("POST", "/", strings.NewReader(`{}`)))
		if rec.Code != 400 {
			t.Fatalf("expected 400, got %d (body=%s)", rec.Code, rec.Body.String())
		}
		var resp HTTPError
		parseJSONResponse(t, rec.Body.Bytes(), &resp)
		if resp.Err != "validation_failed" {
			t.Errorf("expected validation_failed, got %s", resp.Err)
		}
	})

	t.Run("pointer to struct still validated", func(t *testing.T) {
		type reqBody struct {
			Name string `json:"name" validate:"required"`
		}
		handler := H(func(b JSON[*reqBody]) string { return "ok" })
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest("POST", "/", strings.NewReader(`{}`)))
		if rec.Code != 400 {
			t.Fatalf("expected 400, got %d (body=%s)", rec.Code, rec.Body.String())
		}
	})
}

// =============================================================================
// B2: typed-nil errors in (T, error) returns must be treated as success
// =============================================================================

type nilDerefError struct{ msg string }

func (e *nilDerefError) Error() string { return e.msg }

func TestH_TypedNilSecondReturn(t *testing.T) {
	t.Run("typed-nil *HTTPError keeps data", func(t *testing.T) {
		handler := H(func() (User, *HTTPError) {
			return User{Name: "Ann"}, nil
		})
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest("GET", "/", nil))
		if rec.Code != 200 {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		var u User
		parseJSONResponse(t, rec.Body.Bytes(), &u)
		if u.Name != "Ann" {
			t.Errorf("data was dropped; body=%q", rec.Body.String())
		}
	})

	t.Run("typed-nil custom pointer error does not panic", func(t *testing.T) {
		handler := H(func() (string, *nilDerefError) {
			return "ok", nil
		})
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest("GET", "/", nil))
		if rec.Code != 200 || rec.Body.String() != "ok" {
			t.Errorf("expected 200/ok, got %d/%q", rec.Code, rec.Body.String())
		}
	})

	t.Run("typed-nil inside declared error interface keeps data", func(t *testing.T) {
		handler := H(func() (User, error) {
			var e *HTTPError
			return User{Name: "Bea"}, e
		})
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest("GET", "/", nil))
		if rec.Code != 200 {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		var u User
		parseJSONResponse(t, rec.Body.Bytes(), &u)
		if u.Name != "Bea" {
			t.Errorf("data was dropped; body=%q", rec.Body.String())
		}
	})

	t.Run("Result with typed-nil error writes data", func(t *testing.T) {
		var nilErr *HTTPError
		handler := H(func() Result[string] {
			return Result[string]{Data: "ok", Err: nilErr}
		})
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest("GET", "/", nil))
		if rec.Code != 200 || rec.Body.String() != "ok" {
			t.Errorf("expected 200/ok, got %d/%q", rec.Code, rec.Body.String())
		}
	})
}

// =============================================================================
// B3: message visibility must follow the FINAL status code when Result.Code
// overrides the inferred one; explicit *HTTPError keeps its tag and message.
// =============================================================================

func TestResultErr_VisibilityFollowsFinalCode(t *testing.T) {
	Reset()
	defer Reset()

	t.Run("explicit 4xx keeps generic error message", func(t *testing.T) {
		handler := H(func() Result[string] {
			return Err[string](400, errors.New("name is required"))
		})
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest("POST", "/", nil))
		if rec.Code != 400 {
			t.Fatalf("expected 400, got %d", rec.Code)
		}
		var resp HTTPError
		parseJSONResponse(t, rec.Body.Bytes(), &resp)
		if resp.Err != "bad_request" {
			t.Errorf("expected bad_request, got %s", resp.Err)
		}
		if resp.Message != "name is required" {
			t.Errorf("4xx message must be preserved, got %q", resp.Message)
		}
	})

	t.Run("explicit 5xx hides and logs message", func(t *testing.T) {
		var buf bytes.Buffer
		Configure(WithLogger(log.New(&buf, "", 0)))
		defer Reset()

		handler := H(func() Result[string] {
			// "invalid" would infer 400 (message visible); the explicit 500
			// must win and hide it.
			return Err[string](500, errors.New("invalid secret sauce"))
		})
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest("GET", "/", nil))
		if rec.Code != 500 {
			t.Fatalf("expected 500, got %d", rec.Code)
		}
		if strings.Contains(rec.Body.String(), "secret") {
			t.Errorf("5xx body must not leak message; got %s", rec.Body.String())
		}
		if !strings.Contains(buf.String(), "secret") {
			t.Errorf("original 5xx error must be logged; log=%q", buf.String())
		}
	})

	t.Run("override keeps explicit HTTPError tag and message", func(t *testing.T) {
		handler := H(func() Result[string] {
			return Result[string]{
				Code: 503,
				Err:  &HTTPError{Code: 500, Err: "db_down", Message: "database is down"},
			}
		})
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest("GET", "/", nil))
		if rec.Code != 503 {
			t.Fatalf("expected 503, got %d", rec.Code)
		}
		var resp HTTPError
		parseJSONResponse(t, rec.Body.Bytes(), &resp)
		if resp.Err != "db_down" {
			t.Errorf("explicit HTTPError tag must be kept, got %s", resp.Err)
		}
		if resp.Message != "database is down" {
			t.Errorf("explicit HTTPError message must be kept, got %q", resp.Message)
		}
	})

	t.Run("409 gets a conflict tag", func(t *testing.T) {
		handler := H(func() Result[string] {
			return Err[string](409, errors.New("email already exists"))
		})
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest("POST", "/", nil))
		if rec.Code != 409 {
			t.Fatalf("expected 409, got %d", rec.Code)
		}
		var resp HTTPError
		parseJSONResponse(t, rec.Body.Bytes(), &resp)
		if resp.Err != "conflict" {
			t.Errorf("expected conflict, got %s", resp.Err)
		}
		if resp.Message != "email already exists" {
			t.Errorf("4xx message must be preserved, got %q", resp.Message)
		}
	})
}

// =============================================================================
// B4: io.Reader returns that implement io.Closer must be closed after copying
// =============================================================================

type closeTrackingReader struct {
	io.Reader
	closed bool
}

func (c *closeTrackingReader) Close() error {
	c.closed = true
	return nil
}

func TestH_ReaderReturnClosed(t *testing.T) {
	cr := &closeTrackingReader{Reader: strings.NewReader("stream")}
	handler := H(func() io.Reader { return cr })
	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest("GET", "/", nil))

	if rec.Body.String() != "stream" {
		t.Errorf("unexpected body: %s", rec.Body.String())
	}
	if !cr.closed {
		t.Error("reader implementing io.Closer was not closed (fd leak)")
	}
}

// =============================================================================
// B5: inferErrorType must produce meaningful tags beyond the original six
// codes; inferStatusCode learns the keywords the README documents.
// =============================================================================

func TestInferErrorType_UncommonCodes(t *testing.T) {
	tests := []struct {
		code int
		want string
	}{
		{409, "conflict"},
		{422, "unprocessable_entity"},
		{429, "too_many_requests"},
		{503, "service_unavailable"},
		{456, "bad_request"},    // unknown 4xx
		{599, "internal_error"}, // unknown 5xx
	}
	for _, tt := range tests {
		if got := inferErrorType(tt.code); got != tt.want {
			t.Errorf("inferErrorType(%d) = %q, want %q", tt.code, got, tt.want)
		}
	}
}

func TestInferStatusCode_NewKeywords(t *testing.T) {
	tests := []struct {
		msg  string
		want int
	}{
		{"user already exists", 409},
		{"edit conflict detected", 409},
		{"row does not exist", 404},
		{"sorry, it doesn't exist", 404},
		{"validation failed on field x", 400},
	}
	for _, tt := range tests {
		if got := inferStatusCode(tt.msg); got != tt.want {
			t.Errorf("inferStatusCode(%q) = %d, want %d", tt.msg, got, tt.want)
		}
	}
}

// =============================================================================
// B7: Flush must record that headers went out (real servers send an implicit
// 200 on first flush), so a later WriteHeader is treated as a duplicate.
// =============================================================================

func TestResponseWriter_FlushMarksHeaderWritten(t *testing.T) {
	t.Run("flusher underlying", func(t *testing.T) {
		rec := httptest.NewRecorder()
		rw := &ResponseWriter{ResponseWriter: rec}
		rw.Flush()
		if !rw.Written() {
			t.Fatal("Flush must mark headers as written")
		}
		if rw.Status() != http.StatusOK {
			t.Errorf("expected implicit 200, got %d", rw.Status())
		}
		// A later WriteHeader is a duplicate and must be swallowed.
		rw.WriteHeader(500)
		if rec.Code != http.StatusOK {
			t.Errorf("WriteHeader after Flush must be ignored, got %d", rec.Code)
		}
	})

	t.Run("non-flusher underlying stays unwritten", func(t *testing.T) {
		rw := &ResponseWriter{ResponseWriter: plainRW{httptest.NewRecorder()}}
		rw.Flush() // no-op
		if rw.Written() {
			t.Error("no-op Flush must not mark headers as written")
		}
	})
}

// =============================================================================
// B8: decode failures from a custom JSONUnmarshalFunc must map to 400, not 500
// =============================================================================

func TestJSON_DecodeErrorMapping(t *testing.T) {
	Reset()
	defer Reset()

	t.Run("custom unmarshal error is 400 json_decode_error", func(t *testing.T) {
		Configure(WithJSONUnmarshal(func(data []byte, v any) error {
			return errors.New("engine says no")
		}))
		defer Reset()

		handler := H(func(b JSON[User]) string { return "ok" })
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest("POST", "/", strings.NewReader(`{"name":"x"}`)))
		if rec.Code != 400 {
			t.Fatalf("expected 400, got %d (body=%s)", rec.Code, rec.Body.String())
		}
		var resp HTTPError
		parseJSONResponse(t, rec.Body.Bytes(), &resp)
		if resp.Err != "json_decode_error" {
			t.Errorf("expected json_decode_error, got %s", resp.Err)
		}
	})

	t.Run("stdlib syntax error keeps its tag", func(t *testing.T) {
		handler := H(func(b JSON[User]) string { return "ok" })
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest("POST", "/", strings.NewReader(`{bad`)))
		if rec.Code != 400 {
			t.Fatalf("expected 400, got %d", rec.Code)
		}
		var resp HTTPError
		parseJSONResponse(t, rec.Body.Bytes(), &resp)
		if resp.Err != "invalid_json_syntax" {
			t.Errorf("expected invalid_json_syntax, got %s", resp.Err)
		}
	})

	t.Run("stdlib type error keeps its tag", func(t *testing.T) {
		handler := H(func(b JSON[User]) string { return "ok" })
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest("POST", "/", strings.NewReader(`{"age":"x"}`)))
		if rec.Code != 400 {
			t.Fatalf("expected 400, got %d", rec.Code)
		}
		var resp HTTPError
		parseJSONResponse(t, rec.Body.Bytes(), &resp)
		if resp.Err != "invalid_json_type" {
			t.Errorf("expected invalid_json_type, got %s", resp.Err)
		}
	})
}

// =============================================================================
// B9: out-of-range status codes must not panic the request
// =============================================================================

func TestStatusCode_OutOfRangeDoesNotPanic(t *testing.T) {
	t.Run("StatusCode above 999", func(t *testing.T) {
		handler := H(func() StatusCode { return StatusCode(1000) })
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest("GET", "/", nil))
		if rec.Code != 500 {
			t.Errorf("expected coerced 500, got %d", rec.Code)
		}
	})

	t.Run("Result.Code below 100", func(t *testing.T) {
		handler := H(func() Result[string] { return Result[string]{Code: 42, Data: "x"} })
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest("GET", "/", nil))
		if rec.Code != 500 {
			t.Errorf("expected coerced 500, got %d", rec.Code)
		}
	})
}

// =============================================================================
// B10: multipart size overflows must map to 413, not 400 invalid_form
// =============================================================================

func TestFormParseError_MultipartTooLarge(t *testing.T) {
	err := mapFormParseError(multipart.ErrMessageTooLarge)
	var ee *ExtractError
	if !errors.As(err, &ee) || ee.Type != ErrTypeBodyTooLarge {
		t.Fatalf("expected body_too_large ExtractError, got %#v", err)
	}
	if hr := toHTTPError(err); hr.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("expected 413, got %d", hr.Code)
	}
}

// =============================================================================
// PathValue: the constraint must admit every type the README documents
// =============================================================================

func TestPath_NarrowNumericTypes(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /i32/{v}", H(func(v Path[int32]) string { return fmt.Sprint(v.Value) }))
	mux.HandleFunc("GET /u8/{v}", H(func(v Path[uint8]) string { return fmt.Sprint(v.Value) }))
	mux.HandleFunc("GET /f32/{v}", H(func(v Path[float32]) string { return fmt.Sprint(v.Value) }))

	tests := []struct {
		url  string
		code int
		body string
	}{
		{"/i32/-70000", 200, "-70000"},
		{"/u8/255", 200, "255"},
		{"/f32/1.5", 200, "1.5"},
		{"/u8/256", 400, ""}, // overflows uint8
	}
	for _, tt := range tests {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", tt.url, nil))
		if rec.Code != tt.code {
			t.Errorf("%s: expected %d, got %d (body=%s)", tt.url, tt.code, rec.Code, rec.Body.String())
		}
		if tt.code == 200 && rec.Body.String() != tt.body {
			t.Errorf("%s: expected body %q, got %q", tt.url, tt.body, rec.Body.String())
		}
	}
}

// =============================================================================
// B11: a JSON-encoding failure must not surface as an empty 200
// =============================================================================

func TestH_EncodeFailureWrites500(t *testing.T) {
	Reset()
	defer Reset()

	var buf bytes.Buffer
	Configure(WithLogger(log.New(&buf, "", 0)))
	defer Reset()

	handler := H(func() map[string]any {
		return map[string]any{"bad": make(chan int)} // json: unsupported type
	})
	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest("GET", "/", nil))

	if rec.Code != 500 {
		t.Fatalf("expected 500, got %d (body=%q)", rec.Code, rec.Body.String())
	}
	var resp HTTPError
	parseJSONResponse(t, rec.Body.Bytes(), &resp)
	if resp.Err != "internal_error" {
		t.Errorf("expected internal_error, got %s", resp.Err)
	}
	if !strings.Contains(buf.String(), "failed to write response") {
		t.Errorf("encode failure must be logged; log=%q", buf.String())
	}
}

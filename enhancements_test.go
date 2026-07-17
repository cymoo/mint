package m

// Tests for the v1.2.0 enhancement round: context parameters, multipart
// memory configuration, registration-time signature checks, embedded-struct
// file binding, and wildcard path semantics.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func mustPanicAtRegistration(t *testing.T, name string, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Errorf("%s: expected registration panic", name)
		}
	}()
	fn()
}

// =============================================================================
// E1: context.Context handler parameters
// =============================================================================

type ctxTestKey struct{}

func TestH_ContextParam(t *testing.T) {
	t.Run("receives request context", func(t *testing.T) {
		handler := H(func(ctx context.Context) string {
			v, _ := ctx.Value(ctxTestKey{}).(string)
			return v
		})
		req := httptest.NewRequest("GET", "/", nil)
		req = req.WithContext(context.WithValue(req.Context(), ctxTestKey{}, "from-ctx"))
		rec := httptest.NewRecorder()
		handler(rec, req)
		if rec.Body.String() != "from-ctx" {
			t.Errorf("expected context value, got %q", rec.Body.String())
		}
	})

	t.Run("mixes with extractors", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /users/{id}", H(func(ctx context.Context, id Path[int]) string {
			if ctx == nil {
				return "no-ctx"
			}
			return fmt.Sprint(id.Value)
		}))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", "/users/7", nil))
		if rec.Body.String() != "7" {
			t.Errorf("unexpected body: %q", rec.Body.String())
		}
	})
}

// =============================================================================
// E2: WithMultipartMemory
// =============================================================================

func TestWithMultipartMemory(t *testing.T) {
	Reset()
	defer Reset()

	// A 1-byte budget forces file parts to spill to disk; uploads must
	// still work end to end.
	Configure(WithMultipartMemory(1))
	handler := H(func(f File) string { return f.Value.Filename })
	req := createMultipartRequest(t, nil, []testMultipartFile{
		{field: "file", filename: "a.txt", content: "hello world"},
	})
	rec := httptest.NewRecorder()
	handler(rec, req)
	if rec.Code != 200 || rec.Body.String() != "a.txt" {
		t.Errorf("expected 200/a.txt, got %d/%q", rec.Code, rec.Body.String())
	}

	// Values <= 0 fall back to the default.
	Configure(WithMultipartMemory(0))
	if got := multipartMemory(); got != DefaultMultipartMemory {
		t.Errorf("expected fallback to default, got %d", got)
	}
}

// =============================================================================
// E3: custom ResponseWriter interfaces are validated at registration
// =============================================================================

type flushableWriter interface {
	http.ResponseWriter
	http.Flusher
}

type exoticWriter interface {
	http.ResponseWriter
	Exotic()
}

func TestH_CustomWriterInterface(t *testing.T) {
	t.Run("satisfiable superset works", func(t *testing.T) {
		handler := H(func(w flushableWriter) {
			w.Write([]byte("flushed"))
			w.Flush()
		})
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest("GET", "/", nil))
		if rec.Body.String() != "flushed" {
			t.Errorf("unexpected body: %q", rec.Body.String())
		}
		if !rec.Flushed {
			t.Error("expected Flush to reach the recorder")
		}
	})

	t.Run("unsatisfiable superset panics at registration", func(t *testing.T) {
		mustPanicAtRegistration(t, "exoticWriter", func() {
			H(func(w exoticWriter) {})
		})
	})
}

// =============================================================================
// E4: non-struct type parameters panic at registration
// =============================================================================

func TestH_NonStructExtractorPanics(t *testing.T) {
	mustPanicAtRegistration(t, "Header[string]", func() { H(func(h Header[string]) {}) })
	mustPanicAtRegistration(t, "Cookie[int]", func() { H(func(c Cookie[int]) {}) })
	mustPanicAtRegistration(t, "Query[int]", func() { H(func(q Query[int]) {}) })
	mustPanicAtRegistration(t, "Form[[]string]", func() { H(func(f Form[[]string]) {}) })

	// Pointer-to-struct type parameters remain valid.
	_ = H(func(q Query[*QueryParams]) string { return "ok" })
	// JSON has no struct restriction (arrays/maps/scalars are fine).
	_ = H(func(b JSON[[]int]) string { return "ok" })
}

// =============================================================================
// E5: multipart files bind through embedded structs
// =============================================================================

type EmbedBase struct {
	Doc FilePart `schema:"doc"`
}

type EmbedPtrBase struct {
	Extra *FilePart `schema:"extra"`
}

type embedUploadForm struct {
	EmbedBase
	*EmbedPtrBase
	Title string `schema:"title"`
}

func TestForm_EmbeddedStructFileBinding(t *testing.T) {
	Reset()
	defer Reset()

	handler := H(func(f Form[embedUploadForm]) string {
		got := ""
		if f.Value.Doc.Filename != "" {
			got += "doc=" + f.Value.Doc.Filename + ";"
		}
		if f.Value.EmbedPtrBase != nil && f.Value.Extra != nil {
			got += "extra=" + f.Value.Extra.Filename + ";"
		}
		return got + "title=" + f.Value.Title
	})

	req := createMultipartRequest(t,
		map[string]string{"title": "hello"},
		[]testMultipartFile{
			{field: "doc", filename: "doc.txt", content: "d"},
			{field: "extra", filename: "extra.bin", content: "e"},
		})
	rec := httptest.NewRecorder()
	handler(rec, req)

	want := "doc=doc.txt;extra=extra.bin;title=hello"
	if rec.Code != 200 || rec.Body.String() != want {
		t.Errorf("expected %q, got %d/%q", want, rec.Code, rec.Body.String())
	}
}

// =============================================================================
// E8a: conflicting body extractors panic at registration
// =============================================================================

func TestH_BodyConsumerConflicts(t *testing.T) {
	mustPanicAtRegistration(t, "JSON+JSON", func() {
		H(func(a JSON[User], b JSON[User]) {})
	})
	mustPanicAtRegistration(t, "JSON+Form", func() {
		H(func(a JSON[User], b Form[FormData]) {})
	})
	mustPanicAtRegistration(t, "Form+JSON", func() {
		H(func(a Form[FormData], b JSON[User]) {})
	})

	// Form and File share the parsed form; combining them is fine.
	_ = H(func(a Form[FormData], f File) string { return "ok" })
}

// =============================================================================
// E8b: {name...} wildcards may match an empty remainder
// =============================================================================

func TestPath_WildcardEmptyMatch(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /files/{p...}", H(func(p Path[string]) string {
		return "got:" + p.Value
	}))

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/files/a/b/c", nil))
	if rec.Body.String() != "got:a/b/c" {
		t.Errorf("unexpected body: %q", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/files/", nil))
	if rec.Code != 200 || rec.Body.String() != "got:" {
		t.Errorf("empty wildcard match must be 200, got %d/%q", rec.Code, rec.Body.String())
	}
}

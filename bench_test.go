package m

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// benchNopWriter is a ResponseWriter that discards everything, so benchmarks
// measure the framework, not httptest's recorder bookkeeping.
type benchNopWriter struct {
	h http.Header
}

func (w *benchNopWriter) Header() http.Header {
	if w.h == nil {
		w.h = make(http.Header, 4)
	}
	return w.h
}
func (w *benchNopWriter) Write(b []byte) (int, error) { return len(b), nil }
func (w *benchNopWriter) WriteHeader(int)             {}

type benchBody struct {
	Name string `json:"name"`
	Age  int    `json:"age"`
}

// BenchmarkH_JSONEcho measures the common POST path: JSON extract (with
// validation enabled) plus JSON response encoding.
func BenchmarkH_JSONEcho(b *testing.B) {
	Reset()
	defer Reset()

	handler := H(func(body JSON[benchBody]) benchBody { return body.Value })
	payload := `{"name":"alice","age":30}`

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest("POST", "/", strings.NewReader(payload))
		handler(&benchNopWriter{}, req)
	}
}

// BenchmarkH_PathQuery measures a read path: one path param, one query
// struct, plain-text response.
func BenchmarkH_PathQuery(b *testing.B) {
	Reset()
	defer Reset()

	handler := H(func(id Path[int], q Query[QueryParams]) string { return "ok" })

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest("GET", "/users/42?page=2&limit=10", nil)
		req.Pattern = "GET /users/{id}"
		req.SetPathValue("id", "42")
		handler(&benchNopWriter{}, req)
	}
}

// BenchmarkH_NoParams measures pure adapter overhead.
func BenchmarkH_NoParams(b *testing.B) {
	Reset()
	defer Reset()

	handler := H(func() string { return "ok" })

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest("GET", "/", nil)
		handler(&benchNopWriter{}, req)
	}
}

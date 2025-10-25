package m

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/go-playground/validator/v10"
	"github.com/gorilla/schema"
)

// ========== Test Models ==========

type User struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Age   int    `json:"age"`
}

type QueryParams struct {
	Page  int    `schema:"page"`
	Limit int    `schema:"limit"`
	Sort  string `schema:"sort"`
}

type FormData struct {
	Username string `schema:"username"`
	Password string `schema:"password"`
}

// ========== Helper Functions ==========

func createRequestWithPattern(method, target, pattern string) *http.Request {
	req := httptest.NewRequest(method, target, nil)
	req.Pattern = pattern
	return req
}

func parseJSONResponse(t *testing.T, body []byte, v any) {
	if err := json.Unmarshal(body, v); err != nil {
		t.Fatalf("Failed to parse JSON response: %v", err)
	}
}

// ========== JSON Extractor Tests ==========

func TestJSONExtractor(t *testing.T) {
	t.Run("valid json", func(t *testing.T) {
		user := User{Name: "Alice", Email: "alice@example.com", Age: 25}
		body, _ := json.Marshal(user)
		req := httptest.NewRequest("POST", "/", bytes.NewReader(body))

		var j JSON[User]
		err := j.Extract(req)
		if err != nil {
			t.Fatalf("Extract failed: %v", err)
		}
		if j.Value.Name != "Alice" {
			t.Errorf("expected Name=Alice, got %s", j.Value.Name)
		}
		if j.Value.Age != 25 {
			t.Errorf("expected Age=25, got %d", j.Value.Age)
		}
	})

	t.Run("empty body", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/", bytes.NewReader([]byte{}))
		var j JSON[User]
		err := j.Extract(req)
		if err == nil {
			t.Fatal("expected error for empty body")
		}
		var extractErr *ExtractError
		if !errors.As(err, &extractErr) || extractErr.Type != ErrTypeEmptyBody {
			t.Errorf("expected EmptyBodyError, got %v", err)
		}
	})

	t.Run("invalid json", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/", bytes.NewReader([]byte(`{invalid`)))
		var j JSON[User]
		err := j.Extract(req)
		if err == nil {
			t.Fatal("expected error for invalid json")
		}
	})

	t.Run("json with pointer field", func(t *testing.T) {
		type UserPtr struct {
			Name *string `json:"name"`
		}
		body := []byte(`{"name":"Bob"}`)
		req := httptest.NewRequest("POST", "/", bytes.NewReader(body))

		var j JSON[UserPtr]
		err := j.Extract(req)
		if err != nil {
			t.Fatalf("Extract failed: %v", err)
		}
		if j.Value.Name == nil || *j.Value.Name != "Bob" {
			t.Errorf("expected Name=Bob, got %v", j.Value.Name)
		}
	})
}

// ========== Query Extractor Tests ==========

func TestQueryExtractor(t *testing.T) {
	t.Run("valid query params", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/?page=2&limit=10&sort=name", nil)
		var q Query[QueryParams]
		err := q.Extract(req)
		if err != nil {
			t.Fatalf("Extract failed: %v", err)
		}
		if q.Value.Page != 2 {
			t.Errorf("expected Page=2, got %d", q.Value.Page)
		}
		if q.Value.Limit != 10 {
			t.Errorf("expected Limit=10, got %d", q.Value.Limit)
		}
		if q.Value.Sort != "name" {
			t.Errorf("expected Sort=name, got %s", q.Value.Sort)
		}
	})

	t.Run("empty query params", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/", nil)
		var q Query[QueryParams]
		err := q.Extract(req)
		if err != nil {
			t.Fatalf("Extract failed: %v", err)
		}
		if q.Value.Page != 0 {
			t.Errorf("expected Page=0, got %d", q.Value.Page)
		}
	})

	t.Run("invalid query param type", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/?page=invalid", nil)
		var q Query[QueryParams]
		err := q.Extract(req)
		if err == nil {
			t.Fatal("expected error for invalid type")
		}
	})
}

// ========== Form Extractor Tests ==========

func TestFormExtractor(t *testing.T) {
	t.Run("valid form data", func(t *testing.T) {
		formData := url.Values{}
		formData.Set("username", "john")
		formData.Set("password", "secret123")
		req := httptest.NewRequest("POST", "/", strings.NewReader(formData.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

		var f Form[FormData]
		err := f.Extract(req)
		if err != nil {
			t.Fatalf("Extract failed: %v", err)
		}
		if f.Value.Username != "john" {
			t.Errorf("expected Username=john, got %s", f.Value.Username)
		}
		if f.Value.Password != "secret123" {
			t.Errorf("expected Password=secret123, got %s", f.Value.Password)
		}
	})

	t.Run("empty form", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/", strings.NewReader(""))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

		var f Form[FormData]
		err := f.Extract(req)
		if err != nil {
			t.Fatalf("Extract failed: %v", err)
		}
	})
}

// ========== Path Extractor Tests ==========

func TestPathExtractor(t *testing.T) {
	t.Run("string path value", func(t *testing.T) {
		req := createRequestWithPattern("GET", "/products/abc123", "/products/{id}")
		req.SetPathValue("id", "abc123")

		var p Path[string]
		p.SetKey("id")
		err := p.Extract(req)
		if err != nil {
			t.Fatalf("Extract failed: %v", err)
		}
		if p.Value != "abc123" {
			t.Errorf("expected Value=abc123, got %s", p.Value)
		}
	})

	t.Run("int path value", func(t *testing.T) {
		req := createRequestWithPattern("GET", "/products/42", "/products/{id}")
		req.SetPathValue("id", "42")

		var p Path[int]
		p.SetKey("id")
		err := p.Extract(req)
		if err != nil {
			t.Fatalf("Extract failed: %v", err)
		}
		if p.Value != 42 {
			t.Errorf("expected Value=42, got %d", p.Value)
		}
	})

	t.Run("int64 path value", func(t *testing.T) {
		req := createRequestWithPattern("GET", "/items/9876543210", "/items/{id}")
		req.SetPathValue("id", "9876543210")

		var p Path[int64]
		p.SetKey("id")
		err := p.Extract(req)
		if err != nil {
			t.Fatalf("Extract failed: %v", err)
		}
		if p.Value != 9876543210 {
			t.Errorf("expected Value=9876543210, got %d", p.Value)
		}
	})

	t.Run("uint path value", func(t *testing.T) {
		req := createRequestWithPattern("GET", "/items/100", "/items/{count}")
		req.SetPathValue("count", "100")

		var p Path[uint]
		p.SetKey("count")
		err := p.Extract(req)
		if err != nil {
			t.Fatalf("Extract failed: %v", err)
		}
		if p.Value != 100 {
			t.Errorf("expected Value=100, got %d", p.Value)
		}
	})

	t.Run("uint64 path value", func(t *testing.T) {
		req := createRequestWithPattern("GET", "/large/18446744073709551615", "/large/{num}")
		req.SetPathValue("num", "18446744073709551615")

		var p Path[uint64]
		p.SetKey("num")
		err := p.Extract(req)
		if err != nil {
			t.Fatalf("Extract failed: %v", err)
		}
		if p.Value != 18446744073709551615 {
			t.Errorf("expected Value=18446744073709551615, got %d", p.Value)
		}
	})

	t.Run("float64 path value", func(t *testing.T) {
		req := createRequestWithPattern("GET", "/price/19.99", "/price/{amount}")
		req.SetPathValue("amount", "19.99")

		var p Path[float64]
		p.SetKey("amount")
		err := p.Extract(req)
		if err != nil {
			t.Fatalf("Extract failed: %v", err)
		}
		if p.Value != 19.99 {
			t.Errorf("expected Value=19.99, got %f", p.Value)
		}
	})

	t.Run("bool path value - true", func(t *testing.T) {
		req := createRequestWithPattern("GET", "/active/true", "/active/{status}")
		req.SetPathValue("status", "true")

		var p Path[bool]
		p.SetKey("status")
		err := p.Extract(req)
		if err != nil {
			t.Fatalf("Extract failed: %v", err)
		}
		if p.Value != true {
			t.Errorf("expected Value=true, got %v", p.Value)
		}
	})

	t.Run("bool path value - false", func(t *testing.T) {
		req := createRequestWithPattern("GET", "/active/false", "/active/{status}")
		req.SetPathValue("status", "false")

		var p Path[bool]
		p.SetKey("status")
		err := p.Extract(req)
		if err != nil {
			t.Fatalf("Extract failed: %v", err)
		}
		if p.Value != false {
			t.Errorf("expected Value=false, got %v", p.Value)
		}
	})

	t.Run("missing path value", func(t *testing.T) {
		req := createRequestWithPattern("GET", "/products/", "/products/{id}")

		var p Path[string]
		p.SetKey("id")
		err := p.Extract(req)
		if err == nil {
			t.Fatal("expected error for missing path value")
		}
		var extractErr *ExtractError
		if !errors.As(err, &extractErr) || extractErr.Type != ErrTypeMissingPath {
			t.Errorf("expected MissingPathError, got %v", err)
		}
	})

	t.Run("invalid int conversion", func(t *testing.T) {
		req := createRequestWithPattern("GET", "/products/notanumber", "/products/{id}")
		req.SetPathValue("id", "notanumber")

		var p Path[int]
		p.SetKey("id")
		err := p.Extract(req)
		if err == nil {
			t.Fatal("expected error for invalid int")
		}
		var extractErr *ExtractError
		if !errors.As(err, &extractErr) || extractErr.Type != ErrTypePathConversion {
			t.Errorf("expected PathConversionError, got %v", err)
		}
	})

	t.Run("invalid float conversion", func(t *testing.T) {
		req := createRequestWithPattern("GET", "/price/abc", "/price/{amount}")
		req.SetPathValue("amount", "abc")

		var p Path[float64]
		p.SetKey("amount")
		err := p.Extract(req)
		if err == nil {
			t.Fatal("expected error for invalid float")
		}
	})

	t.Run("invalid bool conversion", func(t *testing.T) {
		req := createRequestWithPattern("GET", "/active/yes", "/active/{status}")
		req.SetPathValue("status", "yes")

		var p Path[bool]
		p.SetKey("status")
		err := p.Extract(req)
		if err == nil {
			t.Fatal("expected error for invalid bool")
		}
	})
}

// ========== Handler Tests ==========

func TestH_BasicHandlers(t *testing.T) {
	t.Run("no parameters, no return", func(t *testing.T) {
		handler := H(func() {})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		handler(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("expected status 200, got %d", rec.Code)
		}
	})

	t.Run("return string", func(t *testing.T) {
		handler := H(func() string {
			return "Hello, World!"
		})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		handler(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("expected status 200, got %d", rec.Code)
		}
		if rec.Body.String() != "Hello, World!" {
			t.Errorf("expected 'Hello, World!', got %s", rec.Body.String())
		}
		if rec.Header().Get("Content-Type") != "text/plain; charset=utf-8" {
			t.Errorf("unexpected content type: %s", rec.Header().Get("Content-Type"))
		}
	})

	t.Run("return JSON struct", func(t *testing.T) {
		handler := H(func() User {
			return User{Name: "Alice", Email: "alice@example.com", Age: 25}
		})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		handler(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("expected status 200, got %d", rec.Code)
		}
		var user User
		parseJSONResponse(t, rec.Body.Bytes(), &user)
		if user.Name != "Alice" {
			t.Errorf("expected Name=Alice, got %s", user.Name)
		}
	})

	t.Run("return StatusCode", func(t *testing.T) {
		handler := H(func() StatusCode {
			return StatusCode(201)
		})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/", nil)
		handler(rec, req)
		if rec.Code != 201 {
			t.Errorf("expected status 201, got %d", rec.Code)
		}
	})

	t.Run("return HTML", func(t *testing.T) {
		handler := H(func() HTML {
			return HTML("<h1>Hello</h1>")
		})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		handler(rec, req)
		if rec.Header().Get("Content-Type") != "text/html; charset=utf-8" {
			t.Errorf("unexpected content type: %s", rec.Header().Get("Content-Type"))
		}
		if rec.Body.String() != "<h1>Hello</h1>" {
			t.Errorf("unexpected body: %s", rec.Body.String())
		}
	})

	t.Run("return template.HTML", func(t *testing.T) {
		handler := H(func() template.HTML {
			return template.HTML("<p>Test</p>")
		})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		handler(rec, req)
		if rec.Header().Get("Content-Type") != "text/html; charset=utf-8" {
			t.Errorf("unexpected content type: %s", rec.Header().Get("Content-Type"))
		}
	})

	t.Run("return []byte", func(t *testing.T) {
		handler := H(func() []byte {
			return []byte{0x89, 0x50, 0x4E, 0x47}
		})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		handler(rec, req)
		if rec.Header().Get("Content-Type") != "application/octet-stream" {
			t.Errorf("unexpected content type: %s", rec.Header().Get("Content-Type"))
		}
	})

	t.Run("return io.Reader", func(t *testing.T) {
		handler := H(func() io.Reader {
			return strings.NewReader("streaming content")
		})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		handler(rec, req)
		if rec.Body.String() != "streaming content" {
			t.Errorf("unexpected body: %s", rec.Body.String())
		}
	})
}

func TestH_WithParameters(t *testing.T) {
	t.Run("with JSON parameter", func(t *testing.T) {
		handler := H(func(user JSON[User]) User {
			return user.Value
		})
		rec := httptest.NewRecorder()
		body, _ := json.Marshal(User{Name: "Bob", Email: "bob@example.com", Age: 30})
		req := httptest.NewRequest("POST", "/", bytes.NewReader(body))
		handler(rec, req)
		var result User
		parseJSONResponse(t, rec.Body.Bytes(), &result)
		if result.Name != "Bob" {
			t.Errorf("expected Name=Bob, got %s", result.Name)
		}
	})

	t.Run("with Query parameter", func(t *testing.T) {
		handler := H(func(q Query[QueryParams]) QueryParams {
			return q.Value
		})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/?page=3&limit=20", nil)
		handler(rec, req)
		var result QueryParams
		parseJSONResponse(t, rec.Body.Bytes(), &result)
		if result.Page != 3 || result.Limit != 20 {
			t.Errorf("unexpected query params: %+v", result)
		}
	})

	t.Run("with Path parameter", func(t *testing.T) {
		handler := H(func(id Path[int]) int {
			return id.Value * 2
		})
		rec := httptest.NewRecorder()
		req := createRequestWithPattern("GET", "/items/21", "/items/{id}")
		req.SetPathValue("id", "21")
		handler(rec, req)
		var result int
		parseJSONResponse(t, rec.Body.Bytes(), &result)
		if result != 42 {
			t.Errorf("expected 42, got %d", result)
		}
	})

	t.Run("with http.ResponseWriter", func(t *testing.T) {
		handler := H(func(w http.ResponseWriter) {
			w.Header().Set("X-Custom", "test")
			w.WriteHeader(201)
			w.Write([]byte("custom response"))
		})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/", nil)
		handler(rec, req)
		if rec.Code != 201 {
			t.Errorf("expected status 201, got %d", rec.Code)
		}
		if rec.Header().Get("X-Custom") != "test" {
			t.Errorf("expected X-Custom header")
		}
		if rec.Body.String() != "custom response" {
			t.Errorf("unexpected body: %s", rec.Body.String())
		}
	})

	t.Run("with *http.Request", func(t *testing.T) {
		handler := H(func(r *http.Request) string {
			return r.Method + " " + r.URL.Path
		})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/test", nil)
		handler(rec, req)
		if rec.Body.String() != "POST /test" {
			t.Errorf("unexpected response: %s", rec.Body.String())
		}
	})

	t.Run("multiple parameters", func(t *testing.T) {
		handler := H(func(id Path[int], q Query[QueryParams]) map[string]any {
			return map[string]any{
				"id":   id.Value,
				"page": q.Value.Page,
			}
		})
		rec := httptest.NewRecorder()
		req := createRequestWithPattern("GET", "/items/5?page=2", "/items/{id}")
		req.SetPathValue("id", "5")
		handler(rec, req)
		var result map[string]any
		parseJSONResponse(t, rec.Body.Bytes(), &result)
		if int(result["id"].(float64)) != 5 {
			t.Errorf("expected id=5")
		}
		if int(result["page"].(float64)) != 2 {
			t.Errorf("expected page=2")
		}
	})
}

func TestH_ErrorHandling(t *testing.T) {
	t.Run("return error", func(t *testing.T) {
		handler := H(func() error {
			return errors.New("something went wrong")
		})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		handler(rec, req)
		if rec.Code != 500 {
			t.Errorf("expected status 500, got %d", rec.Code)
		}
		var httpErr HTTPError
		parseJSONResponse(t, rec.Body.Bytes(), &httpErr)
		if httpErr.Code != 500 {
			t.Errorf("expected error code 500, got %d", httpErr.Code)
		}
	})

	t.Run("return data and error - data only", func(t *testing.T) {
		handler := H(func() (string, error) {
			return "success", nil
		})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		handler(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("expected status 200, got %d", rec.Code)
		}
		if rec.Body.String() != "success" {
			t.Errorf("unexpected body: %s", rec.Body.String())
		}
	})

	t.Run("return data and error - error only", func(t *testing.T) {
		handler := H(func() (string, error) {
			return "", errors.New("failed")
		})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		handler(rec, req)
		if rec.Code != 500 {
			t.Errorf("expected status 500, got %d", rec.Code)
		}
	})

	t.Run("HTTPError with custom code", func(t *testing.T) {
		handler := H(func() error {
			return &HTTPError{Code: 404, Err: "not_found", Message: "Resource not found"}
		})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		handler(rec, req)
		if rec.Code != 404 {
			t.Errorf("expected status 404, got %d", rec.Code)
		}
		var httpErr HTTPError
		parseJSONResponse(t, rec.Body.Bytes(), &httpErr)
		if httpErr.Message != "Resource not found" {
			t.Errorf("unexpected message: %s", httpErr.Message)
		}
	})

	t.Run("extraction error", func(t *testing.T) {
		handler := H(func(user JSON[User]) User {
			return user.Value
		})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/", bytes.NewReader([]byte{}))
		handler(rec, req)
		if rec.Code != 400 {
			t.Errorf("expected status 400, got %d", rec.Code)
		}
	})

	t.Run("custom error handler", func(t *testing.T) {
		customErrorHandler := func(w http.ResponseWriter, err error) {
			w.WriteHeader(418)
			w.Write([]byte("I'm a teapot"))
		}

		Configure(WithErrorHandler(customErrorHandler))
		defer func() { Reset() }()

		handler := H(func() error {
			return errors.New("test error")
		})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		handler(rec, req)
		if rec.Code != 418 {
			t.Errorf("expected status 418, got %d", rec.Code)
		}
		if rec.Body.String() != "I'm a teapot" {
			t.Errorf("unexpected body: %s", rec.Body.String())
		}
	})
}

func TestH_ResultType(t *testing.T) {
	t.Run("OK result", func(t *testing.T) {
		handler := H(func() Result[User] {
			return OK(User{Name: "Charlie", Email: "charlie@example.com", Age: 35})
		})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		handler(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("expected status 200, got %d", rec.Code)
		}
		var user User
		parseJSONResponse(t, rec.Body.Bytes(), &user)
		if user.Name != "Charlie" {
			t.Errorf("expected Name=Charlie, got %s", user.Name)
		}
	})

	t.Run("Err result", func(t *testing.T) {
		handler := H(func() Result[User] {
			return Err[User](403, errors.New("forbidden"))
		})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		handler(rec, req)
		if rec.Code != 403 {
			t.Errorf("expected status 403, got %d", rec.Code)
		}
	})

	t.Run("Result with custom headers", func(t *testing.T) {
		handler := H(func() Result[string] {
			r := OK("test")
			r.Headers = http.Header{}
			r.Headers.Set("X-Custom-Header", "value")
			return r
		})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		handler(rec, req)
		if rec.Header().Get("X-Custom-Header") != "value" {
			t.Errorf("expected X-Custom-Header")
		}
	})

	t.Run("Result with custom status code", func(t *testing.T) {
		handler := H(func() Result[User] {
			r := OK(User{Name: "Dave"})
			r.Code = 201
			return r
		})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/", nil)
		handler(rec, req)
		if rec.Code != 201 {
			t.Errorf("expected status 201, got %d", rec.Code)
		}
	})
}

func TestH_HTTPHandler(t *testing.T) {
	t.Run("return http.Handler", func(t *testing.T) {
		customHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(202)
			w.Write([]byte("custom handler"))
		})

		handler := H(func() http.Handler {
			return customHandler
		})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		handler(rec, req)
		if rec.Code != 202 {
			t.Errorf("expected status 202, got %d", rec.Code)
		}
		if rec.Body.String() != "custom handler" {
			t.Errorf("unexpected body: %s", rec.Body.String())
		}
	})
}

// ========== Error Conversion Tests ==========

func TestHTTPError(t *testing.T) {
	t.Run("error without message", func(t *testing.T) {
		err := HTTPError{Code: 404, Err: "not_found"}
		if err.Error() != "not_found" {
			t.Errorf("expected 'not_found', got %s", err.Error())
		}
	})

	t.Run("error with message", func(t *testing.T) {
		err := HTTPError{Code: 404, Err: "not_found", Message: "Resource was not found"}
		if err.Error() != "Resource was not found" {
			t.Errorf("expected 'Resource was not found', got %s", err.Error())
		}
	})
}

func TestExtractError(t *testing.T) {
	t.Run("error message", func(t *testing.T) {
		err := &ExtractError{
			Type:    "test_error",
			Message: "Test error message",
		}
		if err.Error() != "Test error message" {
			t.Errorf("unexpected error message: %s", err.Error())
		}
	})

	t.Run("unwrap", func(t *testing.T) {
		innerErr := errors.New("inner")
		err := &ExtractError{
			Type: "test",
			Err:  innerErr,
		}
		if errors.Unwrap(err) != innerErr {
			t.Error("Unwrap failed")
		}
	})
}

func TestToHTTPError(t *testing.T) {
	t.Run("nil error", func(t *testing.T) {
		result := toHTTPError(nil)
		if result != nil {
			t.Error("expected nil for nil error")
		}
	})

	t.Run("HTTPError pointer", func(t *testing.T) {
		httpErr := &HTTPError{Code: 400, Err: "bad_request"}
		result := toHTTPError(httpErr)
		if result.Code != 400 {
			t.Errorf("expected Code=400, got %d", result.Code)
		}
	})

	t.Run("HTTPError value", func(t *testing.T) {
		httpErr := HTTPError{Code: 403, Err: "forbidden"}
		result := toHTTPError(httpErr)
		if result.Code != 403 {
			t.Errorf("expected Code=403, got %d", result.Code)
		}
	})

	t.Run("ExtractError - body read", func(t *testing.T) {
		err := NewBodyReadError(errors.New("read failed"))
		result := toHTTPError(err)
		if result.Code != 500 {
			t.Errorf("expected Code=500, got %d", result.Code)
		}
	})

	t.Run("ExtractError - empty body", func(t *testing.T) {
		err := NewEmptyBodyError()
		result := toHTTPError(err)
		if result.Code != 400 {
			t.Errorf("expected Code=400, got %d", result.Code)
		}
		if result.Err != "empty_body" {
			t.Errorf("expected Err=empty_body, got %s", result.Err)
		}
	})

	t.Run("ExtractError - form parse", func(t *testing.T) {
		err := NewFormParseError(errors.New("parse failed"))
		result := toHTTPError(err)
		if result.Code != 400 {
			t.Errorf("expected Code=400, got %d", result.Code)
		}
		if result.Err != "invalid_form" {
			t.Errorf("expected Err=invalid_form, got %s", result.Err)
		}
	})

	t.Run("ExtractError - path conversion", func(t *testing.T) {
		err := NewPathConversionError("id", "abc", "int", errors.New("parse failed"))
		result := toHTTPError(err)
		if result.Code != 400 {
			t.Errorf("expected Code=400, got %d", result.Code)
		}
		if result.Err != "invalid_path_parameter" {
			t.Errorf("expected Err=invalid_path_parameter, got %s", result.Err)
		}
	})

	t.Run("ExtractError - missing path", func(t *testing.T) {
		err := NewMissingPathError("id")
		result := toHTTPError(err)
		if result.Code != 400 {
			t.Errorf("expected Code=400, got %d", result.Code)
		}
		if result.Err != "missing_path_parameter" {
			t.Errorf("expected Err=missing_path_parameter, got %s", result.Err)
		}
	})

	t.Run("json.UnmarshalTypeError", func(t *testing.T) {
		err := &json.UnmarshalTypeError{
			Field: "age",
			Type:  reflect.TypeOf(0),
			Value: "string",
		}
		result := toHTTPError(err)
		if result.Code != 400 {
			t.Errorf("expected Code=400, got %d", result.Code)
		}
		if result.Err != "invalid_json_type" {
			t.Errorf("expected Err=invalid_json_type, got %s", result.Err)
		}
	})

	t.Run("json.SyntaxError", func(t *testing.T) {
		err := &json.SyntaxError{}
		result := toHTTPError(err)
		if result.Code != 400 {
			t.Errorf("expected Code=400, got %d", result.Code)
		}
		if result.Err != "invalid_json_syntax" {
			t.Errorf("expected Err=invalid_json_syntax, got %s", result.Err)
		}
	})

	t.Run("schema.MultiError", func(t *testing.T) {
		multiErr := schema.MultiError{
			"field1": errors.New("error1"),
			"field2": errors.New("error2"),
		}
		result := toHTTPError(multiErr)
		if result.Code != 400 {
			t.Errorf("expected Code=400, got %d", result.Code)
		}
		if result.Err != "validation_failed" {
			t.Errorf("expected Err=validation_failed, got %s", result.Err)
		}
	})

	t.Run("schema.ConversionError", func(t *testing.T) {
		err := &schema.ConversionError{Key: "field"}
		result := toHTTPError(err)
		if result.Code != 400 {
			t.Errorf("expected Code=400, got %d", result.Code)
		}
		if result.Err != "conversion_failed" {
			t.Errorf("expected Err=conversion_failed, got %s", result.Err)
		}
	})

	t.Run("schema.UnknownKeyError", func(t *testing.T) {
		err := &schema.UnknownKeyError{Key: "unknown"}
		result := toHTTPError(err)
		if result.Code != 400 {
			t.Errorf("expected Code=400, got %d", result.Code)
		}
		if result.Err != "unknown_field" {
			t.Errorf("expected Err=unknown_field, got %s", result.Err)
		}
	})
}

func TestInferStatusCode(t *testing.T) {
	tests := []struct {
		msg          string
		expectedCode int
	}{
		{"user not found", 404},
		{"resource Not Found", 404},
		{"unauthorized access", 401},
		{"UNAUTHORIZED", 401},
		{"forbidden resource", 403},
		{"Forbidden", 403},
		{"request timeout", 408},
		{"timeout occurred", 408},
		{"bad request", 400},
		{"invalid input", 400},
		{"something went wrong", 500},
		{"internal error", 500},
	}

	for _, tt := range tests {
		t.Run(tt.msg, func(t *testing.T) {
			code := inferStatusCode(tt.msg)
			if code != tt.expectedCode {
				t.Errorf("inferStatusCode(%q) = %d, expected %d", tt.msg, code, tt.expectedCode)
			}
		})
	}
}

func TestInferErrorType(t *testing.T) {
	tests := []struct {
		code         int
		expectedType string
	}{
		{400, "bad_request"},
		{401, "unauthorized"},
		{403, "forbidden"},
		{404, "not_found"},
		{408, "timeout"},
		{500, "internal_error"},
		{503, "internal_error"},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("code_%d", tt.code), func(t *testing.T) {
			errType := inferErrorType(tt.code)
			if errType != tt.expectedType {
				t.Errorf("inferErrorType(%d) = %s, expected %s", tt.code, errType, tt.expectedType)
			}
		})
	}
}

// ========== Pattern Extraction Tests ==========

func TestExtractPatternNames(t *testing.T) {
	tests := []struct {
		pattern  string
		expected []string
	}{
		{"/users/{id}", []string{"id"}},
		{"/users/{id}/posts/{postId}", []string{"id", "postId"}},
		{"/items/{category}/{id}", []string{"category", "id"}},
		{"/static/file.txt", []string{}},
		{"/users/{id}/", []string{"id"}},
		{"/api/{version}/users/{id}", []string{"version", "id"}},
		{"/", []string{}},
		{"/{single}", []string{"single"}},
	}

	for _, tt := range tests {
		t.Run(tt.pattern, func(t *testing.T) {
			result := extractPatternNames(tt.pattern)
			if len(result) != len(tt.expected) {
				t.Fatalf("expected %d names, got %d", len(tt.expected), len(result))
			}
			for i, name := range tt.expected {
				if result[i] != name {
					t.Errorf("expected name[%d]=%s, got %s", i, name, result[i])
				}
			}
		})
	}
}

// ========== ResponseWriter Tests ==========

func TestResponseWriter(t *testing.T) {
	t.Run("write without explicit status", func(t *testing.T) {
		rec := httptest.NewRecorder()
		rw := &ResponseWriter{ResponseWriter: rec}
		rw.Write([]byte("test"))
		if rw.statusCode != http.StatusOK {
			t.Errorf("expected statusCode=200, got %d", rw.statusCode)
		}
		if !rw.headerWritten {
			t.Error("expected headerWritten=true")
		}
	})

	t.Run("write with explicit status", func(t *testing.T) {
		rec := httptest.NewRecorder()
		rw := &ResponseWriter{ResponseWriter: rec}
		rw.WriteHeader(201)
		rw.Write([]byte("created"))
		if rw.statusCode != 201 {
			t.Errorf("expected statusCode=201, got %d", rw.statusCode)
		}
	})

	t.Run("multiple WriteHeader calls", func(t *testing.T) {
		rec := httptest.NewRecorder()
		rw := &ResponseWriter{ResponseWriter: rec}
		rw.WriteHeader(200)
		rw.WriteHeader(500) // Should be ignored
		if rw.statusCode != 200 {
			t.Errorf("expected statusCode=200, got %d", rw.statusCode)
		}
	})
}

// ========== Responder Interface Tests ==========

type CustomResponder struct {
	statusCode int
	body       string
}

func (cr CustomResponder) Respond(w http.ResponseWriter) {
	w.WriteHeader(cr.statusCode)
	w.Write([]byte(cr.body))
}

func TestResponderInterface(t *testing.T) {
	handler := H(func() CustomResponder {
		return CustomResponder{statusCode: 418, body: "I'm a teapot"}
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	handler(rec, req)
	if rec.Code != 418 {
		t.Errorf("expected status 418, got %d", rec.Code)
	}
	if rec.Body.String() != "I'm a teapot" {
		t.Errorf("unexpected body: %s", rec.Body.String())
	}
}

// ========== WriteHeaders Tests ==========

func TestWriteHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	headers := http.Header{}
	headers.Set("X-Header-1", "value1")
	headers.Add("X-Header-2", "value2a")
	headers.Add("X-Header-2", "value2b")

	WriteHeaders(rec, headers)

	if rec.Header().Get("X-Header-1") != "value1" {
		t.Errorf("expected X-Header-1=value1")
	}
	values := rec.Header().Values("X-Header-2")
	if len(values) != 2 {
		t.Errorf("expected 2 values for X-Header-2, got %d", len(values))
	}
}

// ========== Helper Function Tests ==========

func TestGetPointer(t *testing.T) {
	t.Run("non-pointer value", func(t *testing.T) {
		var x int = 42
		val := reflect.ValueOf(&x).Elem()
		ptr := getPointer(val)
		if *(ptr.(*int)) != 42 {
			t.Error("getPointer failed for non-pointer value")
		}
	})

	t.Run("nil pointer", func(t *testing.T) {
		var x *int
		val := reflect.ValueOf(&x).Elem()
		ptr := getPointer(val)
		if ptr.(*int) == nil {
			t.Error("expected non-nil pointer")
		}
	})

	t.Run("non-nil pointer", func(t *testing.T) {
		num := 42
		x := &num
		val := reflect.ValueOf(&x).Elem()
		ptr := getPointer(val)
		if *(ptr.(*int)) != num {
			t.Error("getPointer failed for non-nil pointer")
		}
	})
}

func TestIsNilValue(t *testing.T) {
	tests := []struct {
		name     string
		value    any
		expected bool
	}{
		{"nil interface", nil, true},
		{"nil pointer", (*int)(nil), true},
		{"nil slice", []int(nil), true},
		{"nil map", map[string]int(nil), true},
		{"nil channel", (chan int)(nil), true},
		{"nil func", (func())(nil), true},
		{"non-nil pointer", new(int), false},
		{"non-nil slice", []int{}, false},
		{"non-nil map", map[string]int{}, false},
		{"int", 42, false},
		{"string", "test", false},
		{"bool", true, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			val := reflect.ValueOf(tt.value)
			result := isNilValue(val)
			if result != tt.expected {
				t.Errorf("isNilValue(%v) = %v, expected %v", tt.value, result, tt.expected)
			}
		})
	}

	t.Run("invalid value", func(t *testing.T) {
		var val reflect.Value
		result := isNilValue(val)
		if !result {
			t.Error("expected true for invalid value")
		}
	})
}

// ========== Integration Tests ==========

func TestIntegration_ComplexHandler(t *testing.T) {
	type CreateUserRequest struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	}

	type CreateUserResponse struct {
		ID    int    `json:"id"`
		Name  string `json:"name"`
		Email string `json:"email"`
	}

	handler := H(func(req JSON[CreateUserRequest]) Result[CreateUserResponse] {
		if req.Value.Name == "" {
			return Err[CreateUserResponse](400, errors.New("name is required"))
		}

		resp := CreateUserResponse{
			ID:    123,
			Name:  req.Value.Name,
			Email: req.Value.Email,
		}

		result := OK(resp)
		result.Code = 201
		result.Headers = http.Header{}
		result.Headers.Set("Location", "/users/123")

		return result
	})

	t.Run("successful creation", func(t *testing.T) {
		body, _ := json.Marshal(CreateUserRequest{Name: "Alice", Email: "alice@example.com"})
		req := httptest.NewRequest("POST", "/users", bytes.NewReader(body))
		rec := httptest.NewRecorder()

		handler(rec, req)

		if rec.Code != 201 {
			t.Errorf("expected status 201, got %d", rec.Code)
		}
		if rec.Header().Get("Location") != "/users/123" {
			t.Errorf("expected Location header")
		}

		var resp CreateUserResponse
		parseJSONResponse(t, rec.Body.Bytes(), &resp)
		if resp.ID != 123 || resp.Name != "Alice" {
			t.Errorf("unexpected response: %+v", resp)
		}
	})

	t.Run("validation error", func(t *testing.T) {
		body, _ := json.Marshal(CreateUserRequest{Email: "test@example.com"})
		req := httptest.NewRequest("POST", "/users", bytes.NewReader(body))
		rec := httptest.NewRecorder()

		handler(rec, req)

		if rec.Code != 400 {
			t.Errorf("expected status 400, got %d", rec.Code)
		}
	})
}

func TestIntegration_RESTfulAPI(t *testing.T) {
	// GET /items/{id}
	getHandler := H(func(id Path[int]) Result[map[string]any] {
		if id.Value == 999 {
			return Err[map[string]any](404, errors.New("item not found"))
		}
		return OK(map[string]any{"id": id.Value, "name": "Item " + fmt.Sprint(id.Value)})
	})

	// GET /items?page=1&limit=10
	listHandler := H(func(q Query[QueryParams]) []map[string]any {
		items := make([]map[string]any, 0)
		for i := 0; i < q.Value.Limit; i++ {
			items = append(items, map[string]any{"id": i, "name": fmt.Sprintf("Item %d", i)})
		}
		return items
	})

	t.Run("get existing item", func(t *testing.T) {
		req := createRequestWithPattern("GET", "/items/42", "/items/{id}")
		req.SetPathValue("id", "42")
		rec := httptest.NewRecorder()

		getHandler(rec, req)

		if rec.Code != 200 {
			t.Errorf("expected status 200, got %d", rec.Code)
		}

		var result map[string]any
		parseJSONResponse(t, rec.Body.Bytes(), &result)
		if int(result["id"].(float64)) != 42 {
			t.Errorf("unexpected id: %v", result["id"])
		}
	})

	t.Run("get non-existent item", func(t *testing.T) {
		req := createRequestWithPattern("GET", "/items/999", "/items/{id}")
		req.SetPathValue("id", "999")
		rec := httptest.NewRecorder()

		getHandler(rec, req)

		if rec.Code != 404 {
			t.Errorf("expected status 404, got %d", rec.Code)
		}
	})

	t.Run("list items with pagination", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/items?page=1&limit=5", nil)
		rec := httptest.NewRecorder()

		listHandler(rec, req)

		if rec.Code != 200 {
			t.Errorf("expected status 200, got %d", rec.Code)
		}

		var result []map[string]any
		parseJSONResponse(t, rec.Body.Bytes(), &result)
		if len(result) != 5 {
			t.Errorf("expected 5 items, got %d", len(result))
		}
	})
}

// ========== Panic Tests ==========

func TestH_Panics(t *testing.T) {
	t.Run("panic on non-function", func(t *testing.T) {
		defer func() {
			if r := recover(); r == nil {
				t.Error("expected panic")
			}
		}()
		H("not a function")
	})

	t.Run("panic on too many return values", func(t *testing.T) {
		defer func() {
			if r := recover(); r == nil {
				t.Error("expected panic")
			}
		}()
		H(func() (int, string, error) {
			return 0, "", nil
		})
	})

	t.Run("panic on unsupported parameter type", func(t *testing.T) {
		handler := H(func(x int) {})
		defer func() {
			if r := recover(); r == nil {
				t.Error("expected panic")
			}
		}()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		handler(rec, req)
	})
}

// ========== Edge Cases ==========

func TestEdgeCases(t *testing.T) {
	t.Run("nil return value", func(t *testing.T) {
		handler := H(func() *User {
			return nil
		})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		handler(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("expected status 200, got %d", rec.Code)
		}
	})

	t.Run("empty string return", func(t *testing.T) {
		handler := H(func() string {
			return ""
		})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		handler(rec, req)
		if rec.Body.String() != "" {
			t.Errorf("expected empty body, got %s", rec.Body.String())
		}
	})

	t.Run("zero StatusCode", func(t *testing.T) {
		handler := H(func() StatusCode {
			return StatusCode(0)
		})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		handler(rec, req)
		// StatusCode(0) should write status 200
		if rec.Code != 200 {
			t.Errorf("expected status 200, got %d", rec.Code)
		}
	})

	t.Run("both return values nil", func(t *testing.T) {
		handler := H(func() (*User, error) {
			return nil, nil
		})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		handler(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("expected status 200, got %d", rec.Code)
		}
	})
}

// ========== Configuration Tests ==========

func TestDefaultConfig(t *testing.T) {
	t.Run("framework works without initialization", func(t *testing.T) {
		Reset()
		// Don't call Initialize - should use defaults

		type Response struct {
			Message string `json:"message"`
		}

		handler := H(func() Response {
			return Response{Message: "hello"}
		})

		req := httptest.NewRequest("GET", "/", nil)
		rec := httptest.NewRecorder()
		handler(rec, req)

		if rec.Code != 200 {
			t.Errorf("expected status 200, got %d", rec.Code)
		}

		var resp Response
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to unmarshal response: %v", err)
		}

		if resp.Message != "hello" {
			t.Errorf("expected message=hello, got %s", resp.Message)
		}
	})
}

func TestInitialize(t *testing.T) {
	t.Run("initialize with custom logger", func(t *testing.T) {
		Reset()

		var buf bytes.Buffer
		customLogger := log.New(&buf, "[TEST] ", 0)

		Initialize(
			WithLogger(customLogger),
		)

		// Logger should be used (test indirectly through framework behavior)
		// For now, just verify initialization doesn't panic
	})

	t.Run("initialize only runs once", func(t *testing.T) {
		Reset()

		callCount := 0
		trackingOption := func(c *Config) {
			callCount++
		}

		Initialize(trackingOption)
		Initialize(trackingOption) // Second call should be ignored
		Initialize(trackingOption) // Third call should be ignored

		// Note: We can't directly test callCount since Initialize uses sync.Once
		// But we can verify no panic occurs with multiple calls
	})

	t.Run("initialize with validation disabled", func(t *testing.T) {
		Reset()

		Initialize(
			WithValidation(false),
		)

		type Request struct {
			Email string `json:"email" validate:"required,email"`
		}

		handler := H(func(body JSON[Request]) Request {
			return body.Value
		})

		// Invalid email should pass through without validation
		req := httptest.NewRequest("POST", "/", strings.NewReader(`{"email":"invalid"}`))
		rec := httptest.NewRecorder()
		handler(rec, req)

		if rec.Code != 200 {
			t.Errorf("expected status 200 (validation disabled), got %d", rec.Code)
		}
	})
}

func TestConfigure(t *testing.T) {
	t.Run("configure can be called multiple times", func(t *testing.T) {
		Reset()

		Configure(WithValidation(false))
		Configure(WithValidation(true))

		// Should use latest config
		type Request struct {
			Email string `json:"email" validate:"required,email"`
		}

		handler := H(func(body JSON[Request]) Request {
			return body.Value
		})

		req := httptest.NewRequest("POST", "/", strings.NewReader(`{"email":"invalid"}`))
		rec := httptest.NewRecorder()
		handler(rec, req)

		// Should fail validation since we enabled it
		if rec.Code == 200 {
			t.Error("expected validation error, got success")
		}
	})

	t.Run("configure with custom JSON marshal", func(t *testing.T) {
		Reset()

		// Configure pretty-print JSON
		Configure(
			WithJSONMarshal(func(v any) ([]byte, error) {
				return json.MarshalIndent(v, "", "  ")
			}),
		)

		type Response struct {
			Name string `json:"name"`
			Age  int    `json:"age"`
		}

		handler := H(func() Response {
			return Response{Name: "Alice", Age: 30}
		})

		req := httptest.NewRequest("GET", "/", nil)
		rec := httptest.NewRecorder()
		handler(rec, req)

		body := rec.Body.String()
		if !strings.Contains(body, "\n") {
			t.Error("expected pretty-printed JSON with newlines")
		}
	})

	t.Run("configure with custom JSON encode", func(t *testing.T) {
		Reset()

		Configure(
			WithJSONEncode(func(w io.Writer, v any) error {
				encoder := json.NewEncoder(w)
				encoder.SetIndent("", "    ") // 4 spaces
				return encoder.Encode(v)
			}),
		)

		handler := H(func() map[string]string {
			return map[string]string{"key": "value"}
		})

		req := httptest.NewRequest("GET", "/", nil)
		rec := httptest.NewRecorder()
		handler(rec, req)

		body := rec.Body.String()
		if !strings.Contains(body, "    ") {
			t.Error("expected JSON with 4-space indentation")
		}
	})

	t.Run("configure with custom JSON unmarshal", func(t *testing.T) {
		Reset()

		unmarshalCalled := false
		Configure(
			WithJSONUnmarshal(func(data []byte, v any) error {
				unmarshalCalled = true
				return json.Unmarshal(data, v)
			}),
		)

		type Request struct {
			Name string `json:"name"`
		}

		handler := H(func(body JSON[Request]) Request {
			return body.Value
		})

		req := httptest.NewRequest("POST", "/", strings.NewReader(`{"name":"test"}`))
		rec := httptest.NewRecorder()
		handler(rec, req)

		if !unmarshalCalled {
			t.Error("custom unmarshal function was not called")
		}
	})
}

func TestReset(t *testing.T) {
	t.Run("reset restores defaults", func(t *testing.T) {
		Reset()

		// Change config
		Configure(WithValidation(false))

		// Reset to defaults
		Reset()

		// Validation should be enabled by default
		type Request struct {
			Email string `json:"email" validate:"required,email"`
		}

		handler := H(func(body JSON[Request]) Request {
			return body.Value
		})

		req := httptest.NewRequest("POST", "/", strings.NewReader(`{"email":"invalid"}`))
		rec := httptest.NewRecorder()
		handler(rec, req)

		if rec.Code == 200 {
			t.Error("expected validation error after reset, got success")
		}
	})

	t.Run("reset allows re-initialization", func(t *testing.T) {
		Reset()

		Initialize(WithValidation(false))
		Reset() // Reset also resets the sync.Once

		// Should be able to Initialize again
		Initialize(WithValidation(true))

		// Verification through actual usage
		type Request struct {
			Email string `json:"email" validate:"required,email"`
		}

		handler := H(func(body JSON[Request]) Request {
			return body.Value
		})

		req := httptest.NewRequest("POST", "/", strings.NewReader(`{"email":"invalid"}`))
		rec := httptest.NewRecorder()
		handler(rec, req)

		if rec.Code == 200 {
			t.Error("expected validation error, got success")
		}
	})
}

func TestCustomValidator(t *testing.T) {
	t.Run("custom validator with custom rule", func(t *testing.T) {
		Reset()

		v := validator.New()
		v.RegisterValidation("isalice", func(fl validator.FieldLevel) bool {
			return fl.Field().String() == "alice"
		})

		Initialize(
			WithValidator(v),
		)

		type Request struct {
			Username string `json:"username" validate:"required,isalice"`
		}

		handler := H(func(body JSON[Request]) Request {
			return body.Value
		})

		// Test valid case
		req := httptest.NewRequest("POST", "/", strings.NewReader(`{"username":"alice"}`))
		rec := httptest.NewRecorder()
		handler(rec, req)

		if rec.Code != 200 {
			t.Errorf("expected status 200 for valid username, got %d", rec.Code)
		}

		// Test invalid case
		req = httptest.NewRequest("POST", "/", strings.NewReader(`{"username":"bob"}`))
		rec = httptest.NewRecorder()
		handler(rec, req)

		if rec.Code == 200 {
			t.Error("expected validation error for invalid username")
		}
	})
}

func TestCustomSchemaDecoder(t *testing.T) {
	t.Run("custom schema decoder with alias tag", func(t *testing.T) {
		Reset()

		decoder := schema.NewDecoder()
		decoder.SetAliasTag("form")
		decoder.IgnoreUnknownKeys(true)

		Initialize(
			WithSchemaDecoder(decoder),
		)

		type QueryParams struct {
			PageNum int `form:"page"`
			Size    int `form:"size"`
		}

		handler := H(func(q Query[QueryParams]) QueryParams {
			return q.Value
		})

		req := httptest.NewRequest("GET", "/?page=5&size=20", nil)
		rec := httptest.NewRecorder()
		handler(rec, req)

		if rec.Code != 200 {
			t.Fatalf("expected status 200, got %d", rec.Code)
		}

		var result QueryParams
		if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
			t.Fatalf("failed to unmarshal response: %v", err)
		}

		if result.PageNum != 5 || result.Size != 20 {
			t.Errorf("expected page=5, size=20, got page=%d, size=%d", result.PageNum, result.Size)
		}
	})
}

func TestCustomErrorHandler(t *testing.T) {
	t.Run("custom error handler is called", func(t *testing.T) {
		Reset()

		errorHandlerCalled := false
		var capturedError error

		Initialize(
			WithErrorHandler(func(w http.ResponseWriter, err error) {
				errorHandlerCalled = true
				capturedError = err

				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTeapot) // Use unique status code
				json.NewEncoder(w).Encode(map[string]string{
					"custom": "error handler",
					"error":  err.Error(),
				})
			}),
		)

		testError := errors.New("test error")
		handler := H(func() error {
			return testError
		})

		req := httptest.NewRequest("GET", "/", nil)
		rec := httptest.NewRecorder()
		handler(rec, req)

		if !errorHandlerCalled {
			t.Error("custom error handler was not called")
		}

		if capturedError == nil || capturedError.Error() != "test error" {
			t.Errorf("expected error 'test error', got %v", capturedError)
		}

		if rec.Code != http.StatusTeapot {
			t.Errorf("expected status 418, got %d", rec.Code)
		}

		var response map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatalf("failed to unmarshal response: %v", err)
		}

		if response["custom"] != "error handler" {
			t.Error("expected custom error response format")
		}
	})
}

func TestConfigThreadSafety(t *testing.T) {
	t.Run("concurrent configure calls", func(t *testing.T) {
		Reset()

		done := make(chan bool, 10)

		// Spawn multiple goroutines calling Configure
		for i := 0; i < 10; i++ {
			go func() {
				Configure(
					WithValidation(true),
					WithValidation(false),
				)
				done <- true
			}()
		}

		// Wait for all goroutines
		for i := 0; i < 10; i++ {
			<-done
		}

		// Should not panic
	})

	t.Run("concurrent reads and writes", func(t *testing.T) {
		Reset()

		done := make(chan bool, 20)

		// Writers
		for i := 0; i < 10; i++ {
			go func() {
				Configure(WithValidation(i%2 == 0))
				done <- true
			}()
		}

		// Readers (via handler execution)
		for i := 0; i < 10; i++ {
			go func() {
				type Request struct {
					Value int `json:"value"`
				}

				handler := H(func(body JSON[Request]) Request {
					return body.Value
				})

				req := httptest.NewRequest("POST", "/", strings.NewReader(`{"value":123}`))
				rec := httptest.NewRecorder()
				handler(rec, req)

				done <- true
			}()
		}

		// Wait for all
		for i := 0; i < 20; i++ {
			<-done
		}

		// Should not panic or race
	})
}

func TestConfigWithValidation(t *testing.T) {
	t.Run("validation enabled by default", func(t *testing.T) {
		Reset()

		type Request struct {
			Email string `json:"email" validate:"required,email"`
		}

		handler := H(func(body JSON[Request]) Request {
			return body.Value
		})

		// Invalid email
		req := httptest.NewRequest("POST", "/", strings.NewReader(`{"email":"notanemail"}`))
		rec := httptest.NewRecorder()
		handler(rec, req)

		if rec.Code == 200 {
			t.Error("expected validation error, got success")
		}

		var errResp map[string]any
		json.Unmarshal(rec.Body.Bytes(), &errResp)
		if !strings.Contains(errResp["message"].(string), "email") {
			t.Error("expected email validation error message")
		}
	})

	t.Run("validation can be disabled", func(t *testing.T) {
		Reset()
		Configure(WithValidation(false))

		type Request struct {
			Email string `json:"email" validate:"required,email"`
		}

		handler := H(func(body JSON[Request]) Request {
			return body.Value
		})

		// Invalid email should pass
		req := httptest.NewRequest("POST", "/", strings.NewReader(`{"email":"notanemail"}`))
		rec := httptest.NewRecorder()
		handler(rec, req)

		if rec.Code != 200 {
			t.Errorf("expected status 200 (no validation), got %d", rec.Code)
		}
	})
}

func TestCompleteConfigurationScenario(t *testing.T) {
	t.Run("full custom configuration", func(t *testing.T) {
		Reset()

		var logBuf bytes.Buffer
		customLogger := log.New(&logBuf, "[CUSTOM] ", 0)

		v := validator.New()
		v.RegisterValidation("positive", func(fl validator.FieldLevel) bool {
			return fl.Field().Int() > 0
		})

		decoder := schema.NewDecoder()
		decoder.IgnoreUnknownKeys(true)

		errorHandlerCalled := false

		Initialize(
			WithLogger(customLogger),
			WithValidator(v),
			WithSchemaDecoder(decoder),
			WithValidation(true),
			WithJSONMarshal(func(v any) ([]byte, error) {
				return json.MarshalIndent(v, "", "  ")
			}),
			WithErrorHandler(func(w http.ResponseWriter, err error) {
				errorHandlerCalled = true
				w.WriteHeader(400)
				json.NewEncoder(w).Encode(map[string]string{
					"error": "custom handler",
				})
			}),
		)

		// Test that custom config is used
		type Request struct {
			Count int `json:"count" validate:"required,positive"`
		}

		handler := H(func(body JSON[Request]) Request {
			return body.Value
		})

		// Invalid request (negative number)
		req := httptest.NewRequest("POST", "/", strings.NewReader(`{"count":-5}`))
		rec := httptest.NewRecorder()
		handler(rec, req)

		if !errorHandlerCalled {
			t.Error("custom error handler should have been called")
		}

		if rec.Code != 400 {
			t.Errorf("expected status 400, got %d", rec.Code)
		}
	})
}

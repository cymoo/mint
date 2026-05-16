// Package m (mint) is a small, type-safe Go web toolkit built on top of net/http.
//
// Mint does not provide a router; it relies on the standard library's
// http.ServeMux (Go 1.22+ enhanced patterns). The central abstraction is the
// H(fn) adapter, which turns a regular function into an http.HandlerFunc by
// automatically extracting typed parameters from the request and converting
// the function's return value into an HTTP response.
package m

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"

	"github.com/go-playground/validator/v10"
	"github.com/gorilla/schema"
)

// =============================================================================
// Constants
// =============================================================================

// DefaultMaxRequestBodySize is the default request-body cap (5 MiB).
// It applies to all extractors that read the body (JSON, Form). Override with
// WithMaxRequestBodySize. Set to <= 0 to disable.
const DefaultMaxRequestBodySize int64 = 5 << 20

// ExtractError type names (used by toHTTPError to decide the HTTP status).
const (
	ErrTypeBodyRead       = "body_read_error"
	ErrTypeBodyTooLarge   = "body_too_large"
	ErrTypeEmptyBody      = "empty_body"
	ErrTypeFormParse      = "form_parse_error"
	ErrTypePathConversion = "path_conversion_error"
	ErrTypeMissingPath    = "missing_path_value"
	ErrTypeValidation     = "validation_error"
)

// =============================================================================
// Configuration
// =============================================================================

// Config holds the framework's global configuration.
//
// All fields are optional; defaultConfig() supplies safe defaults.
type Config struct {
	// SchemaDecoder decodes URL query and form values into structs.
	SchemaDecoder *schema.Decoder

	// JSONMarshalFunc serializes a value to JSON bytes. When set, it takes
	// precedence over the default streaming encoder. Setting it via
	// WithJSONMarshal clears JSONEncodeFunc so the two never conflict.
	JSONMarshalFunc func(v any) ([]byte, error)

	// JSONEncodeFunc streams a value as JSON. Used in preference to
	// JSONMarshalFunc when both happen to be set.
	JSONEncodeFunc func(w io.Writer, v any) error

	// JSONUnmarshalFunc decodes JSON bytes into a value.
	JSONUnmarshalFunc func(data []byte, v any) error

	// Logger is used for warnings and 5xx error logs.
	Logger *log.Logger

	// EnableValidation toggles automatic struct validation in JSON, Query,
	// Form, Header and Cookie extractors.
	EnableValidation bool

	// Validator is the validation engine. Auto-created if nil and
	// EnableValidation is true.
	Validator *validator.Validate

	// ErrorHandler, when non-nil, replaces the default JSON error response.
	// Receives the (possibly wrapped) ResponseWriter and the error.
	ErrorHandler func(w http.ResponseWriter, err error)

	// MaxRequestBodySize caps the number of bytes read from r.Body in body
	// consuming extractors (JSON, Form). Defaults to DefaultMaxRequestBodySize.
	// Set to <= 0 to disable.
	MaxRequestBodySize int64
}

// Option is a functional option for configuring the framework.
type Option func(*Config)

// WithSchemaDecoder installs a custom gorilla/schema decoder for query/form.
func WithSchemaDecoder(decoder *schema.Decoder) Option {
	return func(c *Config) { c.SchemaDecoder = decoder }
}

// WithJSONMarshal installs a custom JSON marshaling function.
// This clears any previously set JSONEncodeFunc so behavior is unambiguous.
func WithJSONMarshal(fn func(v any) ([]byte, error)) Option {
	return func(c *Config) {
		c.JSONMarshalFunc = fn
		c.JSONEncodeFunc = nil
	}
}

// WithJSONEncode installs a custom streaming JSON encoder.
func WithJSONEncode(fn func(w io.Writer, v any) error) Option {
	return func(c *Config) { c.JSONEncodeFunc = fn }
}

// WithJSONUnmarshal installs a custom JSON unmarshaling function.
func WithJSONUnmarshal(fn func(data []byte, v any) error) Option {
	return func(c *Config) { c.JSONUnmarshalFunc = fn }
}

// WithLogger installs a custom *log.Logger.
func WithLogger(logger *log.Logger) Option {
	return func(c *Config) { c.Logger = logger }
}

// WithValidation toggles request validation globally.
func WithValidation(enabled bool) Option {
	return func(c *Config) { c.EnableValidation = enabled }
}

// WithValidator installs a custom validator.
func WithValidator(v *validator.Validate) Option {
	return func(c *Config) { c.Validator = v }
}

// WithErrorHandler installs a custom error handler.
func WithErrorHandler(handler func(w http.ResponseWriter, err error)) Option {
	return func(c *Config) { c.ErrorHandler = handler }
}

// WithMaxRequestBodySize sets the maximum request body size in bytes.
// A value <= 0 disables the limit.
func WithMaxRequestBodySize(n int64) Option {
	return func(c *Config) { c.MaxRequestBodySize = n }
}

// defaultConfig returns a Config with sensible defaults.
func defaultConfig() *Config {
	return &Config{
		SchemaDecoder:      newDefaultSchemaDecoder(),
		EnableValidation:   true,
		Validator:          newDefaultValidator(),
		Logger:             log.Default(),
		JSONEncodeFunc:     defaultJSONEncode,
		JSONUnmarshalFunc:  json.Unmarshal,
		MaxRequestBodySize: DefaultMaxRequestBodySize,
	}
}

// defaultJSONEncode marshals v as JSON without escaping <, >, &, and without
// the trailing newline that json.Encoder.Encode adds.
func defaultJSONEncode(w io.Writer, v any) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return err
	}
	b := buf.Bytes()
	if n := len(b); n > 0 && b[n-1] == '\n' {
		b = b[:n-1]
	}
	_, err := w.Write(b)
	return err
}

func newDefaultSchemaDecoder() *schema.Decoder {
	d := schema.NewDecoder()
	d.IgnoreUnknownKeys(true)
	return d
}

func newDefaultValidator() *validator.Validate {
	v := validator.New()
	// Use request-facing tag names so error messages reference what clients sent.
	v.RegisterTagNameFunc(func(fld reflect.StructField) string {
		for _, tag := range []string{"json", "schema", "form", "header", "cookie"} {
			name := strings.SplitN(fld.Tag.Get(tag), ",", 2)[0]
			if name == "-" {
				return ""
			}
			if name != "" {
				return name
			}
		}
		return fld.Name
	})
	return v
}

// configManager manages the global configuration safely across goroutines.
type configManager struct {
	mu          sync.RWMutex
	config      *Config
	once        sync.Once
	initialized bool
}

var global = &configManager{
	config: defaultConfig(),
}

// Initialize installs configuration once at application startup. The first
// call wins; subsequent calls are ignored and emit a warning via the active
// logger. Use Configure for runtime updates and Reset for tests.
func Initialize(opts ...Option) {
	called := false
	global.once.Do(func() {
		called = true
		cfg := defaultConfig()
		for _, opt := range opts {
			opt(cfg)
		}
		if cfg.EnableValidation && cfg.Validator == nil {
			cfg.Validator = newDefaultValidator()
		}
		global.mu.Lock()
		global.config = cfg
		global.initialized = true
		global.mu.Unlock()
	})
	if !called {
		logger().Printf("mint: Initialize called more than once; ignoring subsequent call. Use Configure for runtime updates.")
	}
}

// Configure updates the configuration. Unlike Initialize, it may be called
// multiple times and is safe for runtime tweaks.
func Configure(opts ...Option) {
	global.mu.Lock()
	defer global.mu.Unlock()

	newCfg := *global.config
	for _, opt := range opts {
		opt(&newCfg)
	}
	if newCfg.EnableValidation && newCfg.Validator == nil {
		newCfg.Validator = newDefaultValidator()
	}
	global.config = &newCfg
}

// Reset restores defaults and clears the one-shot Initialize flag.
// Intended for use in tests.
func Reset() {
	global.mu.Lock()
	defer global.mu.Unlock()
	global.config = defaultConfig()
	global.once = sync.Once{}
	global.initialized = false
}

func (cm *configManager) get() *Config {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return cm.config
}

// --- internal accessors -----------------------------------------------------

func logger() *log.Logger {
	if l := global.get().Logger; l != nil {
		return l
	}
	return log.Default()
}

func schemaDecoder() *schema.Decoder {
	if d := global.get().SchemaDecoder; d != nil {
		return d
	}
	return newDefaultSchemaDecoder()
}

func jsonEncode(w io.Writer, v any) error {
	cfg := global.get()

	if cfg.JSONEncodeFunc != nil {
		return cfg.JSONEncodeFunc(w, v)
	}
	if cfg.JSONMarshalFunc != nil {
		b, err := cfg.JSONMarshalFunc(v)
		if err != nil {
			return err
		}
		_, err = w.Write(b)
		return err
	}
	return defaultJSONEncode(w, v)
}

func jsonUnmarshal(data []byte, v any) error {
	if fn := global.get().JSONUnmarshalFunc; fn != nil {
		return fn(data, v)
	}
	return json.Unmarshal(data, v)
}

func validate(v any) error {
	cfg := global.get()
	if !cfg.EnableValidation || cfg.Validator == nil {
		return nil
	}
	return cfg.Validator.Struct(v)
}

func errorHandler() func(w http.ResponseWriter, err error) {
	return global.get().ErrorHandler
}

func maxBodySize() int64 {
	return global.get().MaxRequestBodySize
}

// =============================================================================
// Core types
// =============================================================================

// StatusCode is a numeric return value that sets the response status with an
// empty body. Useful for 201, 202, 204, etc.
type StatusCode int

// HTML is a string typed so the framework serves it as text/html.
type HTML string

// HTTPError is the canonical, JSON-encodable error type. Return one from a
// handler to control the exact status code, error tag, and (optional) message.
type HTTPError struct {
	Code    int    `json:"code"`
	Err     string `json:"error"`
	Message string `json:"message,omitempty"`
}

func (e HTTPError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return e.Err
}

// Result is the most flexible return type: pick a status code, set response
// headers, and either set Data (success) or Err (failure).
type Result[T any] struct {
	Code    int
	Headers http.Header

	Data T
	Err  error
}

func (Result[T]) isResultType() bool { return true }

func (r Result[T]) toResult() Result[any] {
	return Result[any]{
		Code:    r.Code,
		Headers: r.Headers,
		Data:    r.Data,
		Err:     r.Err,
	}
}

// OK returns a successful Result with the given Data and no explicit Code
// (which becomes 200 when written).
func OK[T any](data T) Result[T] { return Result[T]{Data: data} }

// Err returns an error Result with the given status code and error.
func Err[T any](code int, err error) Result[T] { return Result[T]{Code: code, Err: err} }

// Extractor is implemented by types that pull data out of *http.Request.
// User-defined extractors should implement Extract on a pointer receiver.
type Extractor interface {
	Extract(*http.Request) error
}

// KeySetter is implemented by extractors whose binding name (e.g. a path
// parameter) is decided by H() at request time.
type KeySetter interface {
	SetKey(string)
}

// Responder lets a return type fully control how it's written to the response.
type Responder interface {
	Respond(w http.ResponseWriter)
}

// PathValue lists the scalar Go types supported by Path[T].
type PathValue interface {
	~string | ~int | ~int64 | ~uint | ~uint64 | ~float64 | ~bool
}

// =============================================================================
// Built-in extractors
// =============================================================================

// JSON[T] reads a JSON-encoded request body, unmarshals it into T, then
// validates if validation is enabled.
type JSON[T any] struct {
	Value T
}

func (j *JSON[T]) Extract(r *http.Request) error {
	defer r.Body.Close()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		// http.MaxBytesError is reported here when the body exceeds the cap.
		return mapBodyReadError(err)
	}
	if len(body) == 0 {
		return NewEmptyBodyError()
	}

	val := reflect.ValueOf(&j.Value).Elem()
	target := getPointer(val)

	if err := jsonUnmarshal(body, target); err != nil {
		return err
	}
	if err := validate(target); err != nil {
		return NewValidationError(err)
	}
	return nil
}

// Query[T] decodes r.URL.Query() into T using the schema decoder.
type Query[T any] struct {
	Value T
}

func (q *Query[T]) Extract(r *http.Request) error {
	val := reflect.ValueOf(&q.Value).Elem()
	target := getPointer(val)

	if err := schemaDecoder().Decode(target, r.URL.Query()); err != nil {
		return err
	}
	if err := validate(target); err != nil {
		return NewValidationError(err)
	}
	return nil
}

// Form[T] calls r.ParseForm and decodes r.Form into T using the schema decoder.
type Form[T any] struct {
	Value T
}

func (f *Form[T]) Extract(r *http.Request) error {
	if err := r.ParseForm(); err != nil {
		// MaxBytesError surfaces here when the form body exceeds the cap.
		if be := mapBodyReadError(err); be != nil {
			if ee, ok := be.(*ExtractError); ok && ee.Type == ErrTypeBodyTooLarge {
				return be
			}
		}
		return NewFormParseError(err)
	}

	val := reflect.ValueOf(&f.Value).Elem()
	target := getPointer(val)

	if err := schemaDecoder().Decode(target, r.Form); err != nil {
		return err
	}
	if err := validate(target); err != nil {
		return NewValidationError(err)
	}
	return nil
}

// Path[T] extracts a single path parameter from r.PathValue. The binding name
// is set by H() based on positional order of Path[T] parameters in the
// handler signature; see README "Pitfalls" for the implications.
type Path[T PathValue] struct {
	Value T

	key string
}

// SetKey is invoked by the framework. Manually calling it before H() invokes
// the extractor (e.g., in unit tests for an extractor in isolation) is fine.
func (p *Path[T]) SetKey(key string) { p.key = key }

// Key returns the path-parameter name this extractor is bound to.
// Useful for debugging and custom error reporting.
func (p *Path[T]) Key() string { return p.key }

func (p *Path[T]) Extract(r *http.Request) error {
	pv := r.PathValue(p.key)
	if pv == "" {
		return NewMissingPathError(p.key)
	}

	val := reflect.ValueOf(&p.Value).Elem()
	targetType := val.Type().String()

	switch val.Kind() {
	case reflect.String:
		val.SetString(pv)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(pv, 10, val.Type().Bits())
		if err != nil {
			return NewPathConversionError(p.key, pv, targetType, err)
		}
		val.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		n, err := strconv.ParseUint(pv, 10, val.Type().Bits())
		if err != nil {
			return NewPathConversionError(p.key, pv, targetType, err)
		}
		val.SetUint(n)
	case reflect.Float32, reflect.Float64:
		n, err := strconv.ParseFloat(pv, val.Type().Bits())
		if err != nil {
			return NewPathConversionError(p.key, pv, targetType, err)
		}
		val.SetFloat(n)
	case reflect.Bool:
		b, err := strconv.ParseBool(pv)
		if err != nil {
			return NewPathConversionError(p.key, pv, targetType, err)
		}
		val.SetBool(b)
	default:
		return &ExtractError{
			Type:    "unsupported_type",
			Field:   p.key,
			Message: fmt.Sprintf("unsupported path parameter type: %s", targetType),
		}
	}
	return nil
}

// Header[T] reads request headers into a struct. Each exported field is bound
// by an optional `header:"X-My-Name"` tag (defaulting to the field name).
// Missing headers leave fields at their zero value.
//
// Only scalar types are supported on fields (string/int/uint/float/bool and
// their named variants). Validation runs after extraction.
type Header[T any] struct {
	Value T
}

func (h *Header[T]) Extract(r *http.Request) error {
	val := reflect.ValueOf(&h.Value).Elem()
	target := getPointer(val)

	tv := reflect.ValueOf(target).Elem()
	tt := tv.Type()
	if tt.Kind() != reflect.Struct {
		return &ExtractError{
			Type:    "unsupported_type",
			Message: "Header[T] requires T to be a struct type",
		}
	}

	for i := 0; i < tt.NumField(); i++ {
		f := tt.Field(i)
		if !f.IsExported() {
			continue
		}
		name := strings.SplitN(f.Tag.Get("header"), ",", 2)[0]
		if name == "-" {
			continue
		}
		if name == "" {
			name = f.Name
		}

		raw := r.Header.Get(name)
		if raw == "" {
			continue
		}
		if err := setScalarFromString(tv.Field(i), raw); err != nil {
			return &ExtractError{
				Type:    "header_conversion_error",
				Field:   name,
				Value:   raw,
				Message: fmt.Sprintf("invalid header %q: %v", name, err),
				Err:     err,
			}
		}
	}

	if err := validate(target); err != nil {
		return NewValidationError(err)
	}
	return nil
}

// Cookie[T] reads request cookies into a struct. Each exported field is
// bound by an optional `cookie:"name"` tag (defaulting to the field name).
// Missing cookies leave fields at their zero value.
type Cookie[T any] struct {
	Value T
}

func (c *Cookie[T]) Extract(r *http.Request) error {
	val := reflect.ValueOf(&c.Value).Elem()
	target := getPointer(val)

	tv := reflect.ValueOf(target).Elem()
	tt := tv.Type()
	if tt.Kind() != reflect.Struct {
		return &ExtractError{
			Type:    "unsupported_type",
			Message: "Cookie[T] requires T to be a struct type",
		}
	}

	for i := 0; i < tt.NumField(); i++ {
		f := tt.Field(i)
		if !f.IsExported() {
			continue
		}
		name := strings.SplitN(f.Tag.Get("cookie"), ",", 2)[0]
		if name == "-" {
			continue
		}
		if name == "" {
			name = f.Name
		}

		ck, err := r.Cookie(name)
		if err != nil || ck == nil {
			continue
		}
		if err := setScalarFromString(tv.Field(i), ck.Value); err != nil {
			return &ExtractError{
				Type:    "cookie_conversion_error",
				Field:   name,
				Value:   ck.Value,
				Message: fmt.Sprintf("invalid cookie %q: %v", name, err),
				Err:     err,
			}
		}
	}

	if err := validate(target); err != nil {
		return NewValidationError(err)
	}
	return nil
}

func setScalarFromString(v reflect.Value, s string) error {
	if !v.CanSet() {
		return fmt.Errorf("field cannot be set")
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString(s)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(s, 10, v.Type().Bits())
		if err != nil {
			return err
		}
		v.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		n, err := strconv.ParseUint(s, 10, v.Type().Bits())
		if err != nil {
			return err
		}
		v.SetUint(n)
	case reflect.Float32, reflect.Float64:
		n, err := strconv.ParseFloat(s, v.Type().Bits())
		if err != nil {
			return err
		}
		v.SetFloat(n)
	case reflect.Bool:
		b, err := strconv.ParseBool(s)
		if err != nil {
			return err
		}
		v.SetBool(b)
	default:
		return fmt.Errorf("unsupported field type %s", v.Kind())
	}
	return nil
}

// =============================================================================
// ExtractError
// =============================================================================

// ExtractError reports a failure inside an Extractor. The Type field drives
// the HTTP status mapping in toHTTPError.
type ExtractError struct {
	Type    string
	Field   string
	Value   string
	Message string
	Err     error
}

func (e *ExtractError) Error() string { return e.Message }
func (e *ExtractError) Unwrap() error { return e.Err }

func NewBodyReadError(err error) error {
	return &ExtractError{
		Type:    ErrTypeBodyRead,
		Message: "failed to read request body",
		Err:     err,
	}
}

// NewBodyTooLargeError wraps an http.MaxBytesError.
func NewBodyTooLargeError(err error) error {
	limit := int64(0)
	var be *http.MaxBytesError
	if errors.As(err, &be) {
		limit = be.Limit
	}
	msg := "request body too large"
	if limit > 0 {
		msg = fmt.Sprintf("request body too large (limit %d bytes)", limit)
	}
	return &ExtractError{
		Type:    ErrTypeBodyTooLarge,
		Message: msg,
		Err:     err,
	}
}

func NewEmptyBodyError() error {
	return &ExtractError{Type: ErrTypeEmptyBody, Message: "request body is required"}
}

func NewFormParseError(err error) error {
	return &ExtractError{
		Type:    ErrTypeFormParse,
		Message: "invalid form data format",
		Err:     err,
	}
}

func NewPathConversionError(field, value, targetType string, err error) error {
	return &ExtractError{
		Type:    ErrTypePathConversion,
		Field:   field,
		Value:   value,
		Message: fmt.Sprintf("invalid path parameter %q: cannot convert %q to %s", field, value, targetType),
		Err:     err,
	}
}

func NewMissingPathError(field string) error {
	return &ExtractError{
		Type:    ErrTypeMissingPath,
		Field:   field,
		Message: fmt.Sprintf("missing required path parameter: %s", field),
	}
}

func NewValidationError(err error) error {
	return &ExtractError{
		Type:    ErrTypeValidation,
		Message: formatValidationError(err),
		Err:     err,
	}
}

// mapBodyReadError converts a body-read error to the most specific
// ExtractError we can identify.
func mapBodyReadError(err error) error {
	if err == nil {
		return nil
	}
	var be *http.MaxBytesError
	if errors.As(err, &be) {
		return NewBodyTooLargeError(err)
	}
	return NewBodyReadError(err)
}

// =============================================================================
// ResponseWriter wrapper
// =============================================================================

// ResponseWriter wraps http.ResponseWriter so the framework can observe and
// guard the response state (track header writes, prevent double WriteHeader,
// expose the underlying writer via Unwrap and the standard optional
// interfaces: Flusher, Hijacker, Pusher).
type ResponseWriter struct {
	http.ResponseWriter
	statusCode    int
	headerWritten bool
}

func (rw *ResponseWriter) WriteHeader(code int) {
	if rw.headerWritten {
		logger().Printf("mint: multiple calls to WriteHeader (had %d, new %d); ignoring",
			rw.statusCode, code)
		return
	}
	if code <= 0 {
		code = http.StatusOK
	}
	rw.statusCode = code
	rw.headerWritten = true
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *ResponseWriter) Write(b []byte) (int, error) {
	if !rw.headerWritten {
		rw.WriteHeader(http.StatusOK)
	}
	return rw.ResponseWriter.Write(b)
}

// Status returns the status code that was written, or 0 if none.
func (rw *ResponseWriter) Status() int { return rw.statusCode }

// Written reports whether headers have been sent to the client.
func (rw *ResponseWriter) Written() bool { return rw.headerWritten }

// Unwrap returns the underlying http.ResponseWriter so callers (including
// http.NewResponseController) can reach optional interfaces directly.
func (rw *ResponseWriter) Unwrap() http.ResponseWriter { return rw.ResponseWriter }

// Flush implements http.Flusher when the underlying writer supports it. Safe
// to call otherwise (it becomes a no-op), so SSE handlers don't have to
// type-assert before flushing.
func (rw *ResponseWriter) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack implements http.Hijacker when the underlying writer supports it.
func (rw *ResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := rw.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}

// Push implements http.Pusher when the underlying writer supports it.
func (rw *ResponseWriter) Push(target string, opts *http.PushOptions) error {
	if p, ok := rw.ResponseWriter.(http.Pusher); ok {
		return p.Push(target, opts)
	}
	return http.ErrNotSupported
}

// =============================================================================
// H — the central adapter
// =============================================================================

type resultMarker interface {
	isResultType() bool
	toResult() Result[any]
}

var (
	extractorType = reflect.TypeOf((*Extractor)(nil)).Elem()
	errorType     = reflect.TypeOf((*error)(nil)).Elem()
	readerType    = reflect.TypeOf((*io.Reader)(nil)).Elem()

	handlerType        = reflect.TypeOf((*http.Handler)(nil)).Elem()
	responderType      = reflect.TypeOf((*Responder)(nil)).Elem()
	resultMarkerType   = reflect.TypeOf((*resultMarker)(nil)).Elem()
	responseWriterType = reflect.TypeOf((*http.ResponseWriter)(nil)).Elem()
	httpRequestType    = reflect.TypeOf((*http.Request)(nil))
)

// H adapts fn into an http.HandlerFunc.
//
// The handler may have zero or more parameters; each parameter must be one of:
//   - *http.Request
//   - http.ResponseWriter
//   - a value type whose pointer implements Extractor
//
// It may return zero, one, or two values:
//   - 0 values  → empty 200 response
//   - 1 value   → see "Response Types" in the README
//   - 2 values  → (T, error) where T is a struct/scalar/http.Handler/io.Reader/Responder
//
// Misuse panics at registration time so problems surface before the server
// starts serving traffic.
func H(fn any) http.HandlerFunc {
	if fn == nil {
		panic("mint.H: handler must be a function, got <nil>")
	}

	fnVal := reflect.ValueOf(fn)
	fnType := fnVal.Type()

	if fnType.Kind() != reflect.Func {
		panic(fmt.Sprintf("mint.H: handler must be a function, got %T", fn))
	}
	if fnVal.IsNil() {
		panic("mint.H: handler function must not be nil")
	}

	paramTypes := make([]reflect.Type, fnType.NumIn())
	for i := 0; i < fnType.NumIn(); i++ {
		paramTypes[i] = fnType.In(i)
		if !isSupportedParamType(paramTypes[i]) {
			panic(fmt.Sprintf("mint.H: unsupported parameter type %s at position %d",
				paramTypes[i].String(), i))
		}
	}

	numOut := fnType.NumOut()
	if numOut > 2 {
		panic(fmt.Sprintf("mint.H: handler can return at most 2 values, got %d", numOut))
	}

	if numOut == 1 {
		rt := fnType.Out(0)
		if rt.Kind() == reflect.Interface && !isSupportedSingleInterfaceReturn(rt) {
			panic("mint.H: interface return type must implement error, http.Handler, io.Reader, or Responder")
		}
	}

	if numOut == 2 {
		rt1 := fnType.Out(0)
		rt2 := fnType.Out(1)

		if rt1.Kind() == reflect.Interface && !isSupportedDataInterfaceReturn(rt1) {
			panic("mint.H: first return interface must implement http.Handler, io.Reader, or Responder")
		}
		if rt1.Implements(resultMarkerType) {
			panic("mint.H: first return value cannot be Result[T] when returning two values")
		}
		if !rt2.Implements(errorType) {
			panic(fmt.Sprintf("mint.H: second return value must be error, got %s", rt2.String()))
		}
	}

	return func(w http.ResponseWriter, r *http.Request) {
		rw := &ResponseWriter{ResponseWriter: w}

		// Enforce the body size cap once per request, before any extractor reads.
		if maxN := maxBodySize(); maxN > 0 {
			r.Body = http.MaxBytesReader(rw, r.Body, maxN)
		}

		args := make([]reflect.Value, len(paramTypes))
		pathKeys := patternNames(r.Pattern)
		keyIdx := 0

		for i, paramType := range paramTypes {
			switch {
			case isExtractorParam(paramType):
				paramVal := reflect.New(paramType).Elem()
				extractor := paramVal.Addr().Interface().(Extractor)

				if ks, ok := extractor.(KeySetter); ok {
					if keyIdx >= len(pathKeys) {
						// Pattern has fewer {placeholders} than Path-style
						// extractors in this handler. Surface a clear error
						// to the client and log a one-time warning instead
						// of crashing the server.
						warnPathMismatch(r.Pattern)
						e := handleError(rw, &ExtractError{
							Type:    ErrTypeMissingPath,
							Message: "handler expects more path parameters than the route pattern provides",
						})
						if e != nil {
							logger().Printf("mint: failed to write error response: %v", e)
						}
						return
					}
					ks.SetKey(pathKeys[keyIdx])
					keyIdx++
				}

				if err := extractor.Extract(r); err != nil {
					if e := handleError(rw, err); e != nil {
						logger().Printf("mint: failed to write error response: %v", e)
					}
					return
				}
				args[i] = paramVal

			case paramType.Kind() == reflect.Interface && paramType.Implements(responseWriterType):
				args[i] = reflect.ValueOf(rw)

			case paramType == httpRequestType:
				args[i] = reflect.ValueOf(r)

			default:
				panic(fmt.Sprintf("mint.H: unsupported parameter type %s at position %d",
					paramType.String(), i))
			}
		}

		results := fnVal.Call(args)

		switch len(results) {
		case 0:
			return
		case 1:
			if isNilValue(results[0]) {
				return
			}
			rv := results[0].Interface()
			if handler, ok := rv.(http.Handler); ok {
				handler.ServeHTTP(rw, r)
				return
			}
			if err := handleOneResult(rw, rv); err != nil {
				logger().Printf("mint: failed to write response: %v", err)
			}
		case 2:
			if isNilValue(results[0]) && isNilValue(results[1]) {
				return
			}
			rv := results[0].Interface()
			errVal := results[1].Interface()
			if errVal == nil {
				if handler, ok := rv.(http.Handler); ok {
					handler.ServeHTTP(rw, r)
					return
				}
			}
			if err := handleTwoResults(rw, rv, errVal); err != nil {
				logger().Printf("mint: failed to write response: %v", err)
			}
		}
	}
}

func isExtractorParam(paramType reflect.Type) bool {
	return reflect.PointerTo(paramType).Implements(extractorType)
}

func isSupportedParamType(paramType reflect.Type) bool {
	return isExtractorParam(paramType) ||
		paramType == httpRequestType ||
		(paramType.Kind() == reflect.Interface && paramType.Implements(responseWriterType))
}

func isSupportedSingleInterfaceReturn(rt reflect.Type) bool {
	return rt.Implements(errorType) ||
		rt.Implements(handlerType) ||
		rt.Implements(readerType) ||
		rt.Implements(responderType)
}

func isSupportedDataInterfaceReturn(rt reflect.Type) bool {
	return rt.Implements(handlerType) ||
		rt.Implements(readerType) ||
		rt.Implements(responderType)
}

// =============================================================================
// Header helpers and result handling
// =============================================================================

// WriteHeaders adds every (key, value) pair from src to w.Header().
func WriteHeaders(w http.ResponseWriter, src http.Header) {
	for key, values := range src {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
}

func setHeaderIfMissing(w http.ResponseWriter, key, value string) {
	if w.Header().Get(key) == "" {
		w.Header().Set(key, value)
	}
}

// formatValidationError turns a validator.ValidationErrors into one readable
// "; "-separated message using the request-facing field names.
func formatValidationError(err error) string {
	var ve validator.ValidationErrors
	if !errors.As(err, &ve) {
		return err.Error()
	}
	if len(ve) == 0 {
		return "validation failed"
	}

	msgs := make([]string, 0, len(ve))
	for _, fe := range ve {
		field := fe.Field()
		if field == "" {
			field = fe.StructField()
		}
		msgs = append(msgs, formatFieldError(field, fe))
	}
	return strings.Join(msgs, "; ")
}

func formatFieldError(field string, fe validator.FieldError) string {
	switch fe.Tag() {
	case "required":
		return fmt.Sprintf("%s is required", field)
	case "email":
		return fmt.Sprintf("%s must be a valid email address", field)
	case "min":
		return fmt.Sprintf("%s must be at least %s", field, fe.Param())
	case "max":
		return fmt.Sprintf("%s must be at most %s", field, fe.Param())
	case "len":
		return fmt.Sprintf("%s must be %s characters long", field, fe.Param())
	case "gt":
		return fmt.Sprintf("%s must be greater than %s", field, fe.Param())
	case "gte":
		return fmt.Sprintf("%s must be greater than or equal to %s", field, fe.Param())
	case "lt":
		return fmt.Sprintf("%s must be less than %s", field, fe.Param())
	case "lte":
		return fmt.Sprintf("%s must be less than or equal to %s", field, fe.Param())
	case "oneof":
		return fmt.Sprintf("%s must be one of [%s]", field, fe.Param())
	case "url":
		return fmt.Sprintf("%s must be a valid URL", field)
	case "uri":
		return fmt.Sprintf("%s must be a valid URI", field)
	case "alpha":
		return fmt.Sprintf("%s must contain only letters", field)
	case "alphanum":
		return fmt.Sprintf("%s must contain only letters and numbers", field)
	case "numeric":
		return fmt.Sprintf("%s must be numeric", field)
	case "uuid":
		return fmt.Sprintf("%s must be a valid UUID", field)
	default:
		return fmt.Sprintf("%s failed validation (%s)", field, fe.Tag())
	}
}

func getPointer(val reflect.Value) any {
	if val.Type().Kind() == reflect.Ptr {
		if val.IsNil() {
			val.Set(reflect.New(val.Type().Elem()))
		}
		return val.Interface()
	}
	return val.Addr().Interface()
}

func isNilValue(v reflect.Value) bool {
	if !v.IsValid() {
		return true
	}
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Ptr, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

func handleOneResult(w http.ResponseWriter, data any) error {
	switch v := data.(type) {
	case resultMarker:
		return handleResult(w, v.toResult())
	case error:
		return handleError(w, v)
	default:
		return handleCommonTypes(w, data)
	}
}

func handleTwoResults(w http.ResponseWriter, data any, err any) error {
	if err != nil {
		return handleError(w, err.(error))
	}
	return handleCommonTypes(w, data)
}

func handleCommonTypes(w http.ResponseWriter, data any) error {
	if data == nil {
		return nil
	}
	if responder, ok := data.(Responder); ok {
		responder.Respond(w)
		return nil
	}

	switch v := data.(type) {
	case string:
		setHeaderIfMissing(w, "Content-Type", "text/plain; charset=utf-8")
		_, err := fmt.Fprint(w, v)
		return err
	case StatusCode:
		w.WriteHeader(int(v))
		return nil
	case []byte:
		setHeaderIfMissing(w, "Content-Type", "application/octet-stream")
		_, err := w.Write(v)
		return err
	case HTML, template.HTML:
		setHeaderIfMissing(w, "Content-Type", "text/html; charset=utf-8")
		_, err := fmt.Fprint(w, v)
		return err
	case io.Reader:
		_, err := io.Copy(w, v)
		return err
	default:
		setHeaderIfMissing(w, "Content-Type", "application/json; charset=utf-8")
		return jsonEncode(w, data)
	}
}

func handleResult(w http.ResponseWriter, result Result[any]) error {
	if result.Headers != nil {
		WriteHeaders(w, result.Headers)
	}
	if result.Err != nil {
		return handleErrorWithCode(w, result.Err, result.Code)
	}
	if result.Code != 0 {
		w.WriteHeader(result.Code)
	}
	return handleCommonTypes(w, result.Data)
}

func handleError(w http.ResponseWriter, err error) error {
	return handleErrorWithCode(w, err, 0)
}

func handleErrorWithCode(w http.ResponseWriter, err error, codeOverride int) error {
	if eh := errorHandler(); eh != nil {
		eh(w, err)
		return nil
	}

	statusWritten := false
	if rw, ok := w.(*ResponseWriter); ok {
		statusWritten = rw.headerWritten
	}

	httpErr := toHTTPError(err)
	if httpErr == nil {
		return nil
	}

	if codeOverride != 0 {
		httpErr = &HTTPError{
			Code:    codeOverride,
			Err:     inferErrorType(codeOverride),
			Message: httpErr.Message,
		}
	}

	setHeaderIfMissing(w, "Content-Type", "application/json; charset=utf-8")
	if !statusWritten {
		w.WriteHeader(httpErr.Code)
	}

	if httpErr.Code >= 500 {
		// Log the *original* error for 5xx; the response body intentionally
		// omits Message to avoid leaking internals.
		logger().Printf("mint: 5xx: %v", err)
	}

	return jsonEncode(w, httpErr)
}

// toHTTPError converts an arbitrary error to a JSON-encodable HTTPError.
//
// Visibility policy: 4xx responses include Message (clients can see what they
// did wrong). 5xx responses hide Message — the original error is logged
// instead, so internal details don't leak.
func toHTTPError(err error) *HTTPError {
	if err == nil {
		return nil
	}

	// 1. Explicit HTTPError wins.
	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		return httpErr
	}
	var httpErrVal HTTPError
	if errors.As(err, &httpErrVal) {
		return &httpErrVal
	}

	// 2. ExtractError → fixed mapping per Type.
	var extractErr *ExtractError
	if errors.As(err, &extractErr) {
		return extractErrorToHTTP(extractErr)
	}

	// 3. Well-known stdlib / dependency error types.
	switch e := err.(type) {
	case *http.MaxBytesError:
		return &HTTPError{
			Code:    http.StatusRequestEntityTooLarge,
			Err:     "body_too_large",
			Message: fmt.Sprintf("request body exceeds %d bytes", e.Limit),
		}
	case *json.UnmarshalTypeError:
		return &HTTPError{
			Code:    http.StatusBadRequest,
			Err:     "invalid_json_type",
			Message: fmt.Sprintf("field %q expects %s but got %s", e.Field, e.Type.String(), e.Value),
		}
	case *json.SyntaxError:
		return &HTTPError{
			Code:    http.StatusBadRequest,
			Err:     "invalid_json_syntax",
			Message: "invalid JSON syntax",
		}
	case schema.MultiError:
		msgs := make([]string, 0, len(e))
		for field, fieldErr := range e {
			msgs = append(msgs, fmt.Sprintf("%s: %s", field, fieldErr.Error()))
		}
		return &HTTPError{
			Code:    http.StatusBadRequest,
			Err:     "validation_failed",
			Message: strings.Join(msgs, "; "),
		}
	case *schema.ConversionError:
		return &HTTPError{
			Code:    http.StatusBadRequest,
			Err:     "conversion_failed",
			Message: fmt.Sprintf("cannot convert field %q", e.Key),
		}
	case *schema.UnknownKeyError:
		return &HTTPError{
			Code:    http.StatusBadRequest,
			Err:     "unknown_field",
			Message: fmt.Sprintf("unknown field: %s", e.Key),
		}
	}

	// 4. Last resort: infer status from the error message and preserve the
	// message for 4xx so clients can act on it; hide it for 5xx.
	msg := err.Error()
	code := inferStatusCode(msg)
	out := &HTTPError{Code: code, Err: inferErrorType(code)}
	if code >= 400 && code < 500 {
		out.Message = msg
	}
	return out
}

func extractErrorToHTTP(e *ExtractError) *HTTPError {
	switch e.Type {
	case ErrTypeBodyRead:
		return &HTTPError{
			Code: http.StatusInternalServerError,
			Err:  "internal_server_error",
			// no Message: hide internals for 5xx
		}
	case ErrTypeBodyTooLarge:
		return &HTTPError{
			Code:    http.StatusRequestEntityTooLarge,
			Err:     "body_too_large",
			Message: e.Message,
		}
	case ErrTypeEmptyBody:
		return &HTTPError{Code: http.StatusBadRequest, Err: "empty_body", Message: e.Message}
	case ErrTypeFormParse:
		return &HTTPError{Code: http.StatusBadRequest, Err: "invalid_form", Message: e.Message}
	case ErrTypePathConversion:
		return &HTTPError{Code: http.StatusBadRequest, Err: "invalid_path_parameter", Message: e.Message}
	case ErrTypeMissingPath:
		return &HTTPError{Code: http.StatusBadRequest, Err: "missing_path_parameter", Message: e.Message}
	case ErrTypeValidation:
		return &HTTPError{Code: http.StatusBadRequest, Err: "validation_failed", Message: e.Message}
	default:
		return &HTTPError{Code: http.StatusBadRequest, Err: e.Type, Message: e.Message}
	}
}

func inferStatusCode(msg string) int {
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "not found"):
		return http.StatusNotFound
	case strings.Contains(lower, "unauthorized"):
		return http.StatusUnauthorized
	case strings.Contains(lower, "forbidden"):
		return http.StatusForbidden
	case strings.Contains(lower, "timeout"):
		return http.StatusRequestTimeout
	case strings.Contains(lower, "bad request"), strings.Contains(lower, "invalid"):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

func inferErrorType(code int) string {
	switch code {
	case http.StatusBadRequest:
		return "bad_request"
	case http.StatusUnauthorized:
		return "unauthorized"
	case http.StatusForbidden:
		return "forbidden"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusRequestTimeout:
		return "timeout"
	case http.StatusRequestEntityTooLarge:
		return "body_too_large"
	default:
		return "internal_error"
	}
}

// =============================================================================
// Pattern parsing
// =============================================================================

var (
	patternCache   sync.Map // map[string][]string
	patternWarnMu  sync.Mutex
	patternWarnSet = map[string]struct{}{}
)

// patternNames returns the path-parameter names from a Go 1.22+ pattern. The
// result is cached because the same pattern repeats every request.
func patternNames(pattern string) []string {
	if v, ok := patternCache.Load(pattern); ok {
		return v.([]string)
	}
	names := extractPatternNames(pattern)
	patternCache.Store(pattern, names)
	return names
}

func warnPathMismatch(pattern string) {
	patternWarnMu.Lock()
	_, already := patternWarnSet[pattern]
	if !already {
		patternWarnSet[pattern] = struct{}{}
	}
	patternWarnMu.Unlock()
	if !already {
		logger().Printf("mint: handler has more Path[T] parameters than the route pattern %q provides", pattern)
	}
}

// extractPatternNames parses a pattern like "GET /users/{id}/posts/{pid}" and
// returns ["id", "pid"]. The "{$}" end-of-path marker and the "..." catch-all
// suffix are stripped. Unbalanced braces emit a logger warning.
func extractPatternNames(pattern string) []string {
	names := []string{}
	inParam := false
	current := ""
	depth := 0

	for i, ch := range pattern {
		switch {
		case ch == '{':
			if inParam {
				logger().Printf("mint: nested braces at position %d in pattern %q", i, pattern)
			}
			inParam = true
			depth++
			current = ""
		case ch == '}':
			if !inParam {
				logger().Printf("mint: unmatched closing brace at position %d in pattern %q", i, pattern)
				continue
			}
			inParam = false
			depth--
			name := strings.TrimSuffix(current, "...")
			if name != "" && name != "$" {
				names = append(names, name)
			}
		case inParam:
			current += string(ch)
		}
	}
	if depth != 0 {
		logger().Printf("mint: unbalanced braces in pattern %q", pattern)
	}
	return names
}

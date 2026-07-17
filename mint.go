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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

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

// DefaultMultipartMemory is the in-memory budget passed to
// Request.ParseMultipartForm. File content beyond this threshold is stored in
// temporary files by the standard library.
const DefaultMultipartMemory int64 = 32 << 20

// ExtractError type names (used by toHTTPError to decide the HTTP status).
const (
	ErrTypeBodyRead       = "body_read_error"
	ErrTypeBodyTooLarge   = "body_too_large"
	ErrTypeEmptyBody      = "empty_body"
	ErrTypeJSONDecode     = "json_decode_error"
	ErrTypeFormParse      = "form_parse_error"
	ErrTypeFileMissing    = "file_missing"
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

	// MultipartMemory is the in-memory budget passed to ParseMultipartForm
	// for multipart requests; parts beyond it spill to temporary files.
	// Values <= 0 fall back to DefaultMultipartMemory (32 MiB).
	MultipartMemory int64
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

// WithMultipartMemory sets the in-memory budget for parsing multipart forms.
// A value <= 0 falls back to DefaultMultipartMemory.
func WithMultipartMemory(n int64) Option {
	return func(c *Config) { c.MultipartMemory = n }
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
		MultipartMemory:    DefaultMultipartMemory,
	}
}

// jsonBufPool recycles encode buffers across requests. Buffers that grew
// beyond maxPooledBufferSize are dropped so one huge response doesn't pin
// memory forever.
var jsonBufPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}

const maxPooledBufferSize = 1 << 20 // 1 MiB

// defaultJSONEncode marshals v as JSON without escaping <, >, &, and without
// the trailing newline that json.Encoder.Encode adds.
func defaultJSONEncode(w io.Writer, v any) error {
	buf := jsonBufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer func() {
		if buf.Cap() <= maxPooledBufferSize {
			jsonBufPool.Put(buf)
		}
	}()

	enc := json.NewEncoder(buf)
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

// configManager manages the global configuration. Reads are lock-free
// (atomic pointer load, requests never contend); writers are serialized by mu
// and publish a fresh *Config, so a Config is immutable once stored.
type configManager struct {
	mu     sync.Mutex // serializes Initialize/Configure/Reset
	config atomic.Pointer[Config]
	once   sync.Once
}

var global = newConfigManager()

func newConfigManager() *configManager {
	cm := &configManager{}
	cm.config.Store(defaultConfig())
	return cm
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
		global.config.Store(cfg)
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

	newCfg := *global.config.Load()
	for _, opt := range opts {
		opt(&newCfg)
	}
	if newCfg.EnableValidation && newCfg.Validator == nil {
		newCfg.Validator = newDefaultValidator()
	}
	global.config.Store(&newCfg)
}

// Reset restores defaults and clears the one-shot Initialize flag.
// Intended for use in tests.
func Reset() {
	global.mu.Lock()
	defer global.mu.Unlock()
	global.config.Store(defaultConfig())
	global.once = sync.Once{}
}

func (cm *configManager) get() *Config {
	return cm.config.Load()
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
	rv := reflect.ValueOf(v)
	for rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return nil
		}
		rv = rv.Elem()
	}
	// validator.Struct only accepts struct types (and rejects time.Time
	// convertible ones); anything else — slices, maps, scalars — would come
	// back as InvalidValidationError and fail perfectly valid requests.
	if rv.Kind() != reflect.Struct || rv.Type().ConvertibleTo(timeType) {
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

func multipartMemory() int64 {
	if n := global.get().MultipartMemory; n > 0 {
		return n
	}
	return DefaultMultipartMemory
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

// registrationChecker lets built-in extractors validate their type parameter
// when the handler is registered, so misconfigured handlers panic at startup
// instead of failing every request.
type registrationChecker interface {
	checkRegistration() error
}

// wildcardSetter marks a Path extractor as bound to a {name...} wildcard,
// for which an empty remainder is a legitimate match.
type wildcardSetter interface {
	setWildcard()
}

// rawBodyConsumer / parsedFormConsumer tag the built-in extractors that read
// the request body, so H() can reject handlers that would consume it twice.
type rawBodyConsumer interface{ consumesRawBody() }
type parsedFormConsumer interface{ consumesParsedForm() }

// requireStructT enforces that an extractor's type parameter is a struct
// (possibly behind pointers).
func requireStructT[T any](extractor string) error {
	t := reflect.TypeFor[T]()
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return fmt.Errorf("%s requires a struct type parameter, got %s",
			extractor, reflect.TypeFor[T]().String())
	}
	return nil
}

// Responder lets a return type fully control how it's written to the response.
type Responder interface {
	Respond(w http.ResponseWriter)
}

// PathValue lists the scalar Go types supported by Path[T].
type PathValue interface {
	~string |
		~int | ~int8 | ~int16 | ~int32 | ~int64 |
		~uint | ~uint8 | ~uint16 | ~uint32 | ~uint64 | ~uintptr |
		~float32 | ~float64 | ~bool
}

// =============================================================================
// Built-in extractors
// =============================================================================

// JSON[T] reads a JSON-encoded request body, unmarshals it into T, then
// validates if validation is enabled.
type JSON[T any] struct {
	Value T
}

func (*JSON[T]) consumesRawBody() {}

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
		// Wrap so unknown error types from a custom JSONUnmarshalFunc still
		// map to 400 instead of falling through to the 500 heuristics.
		return NewJSONDecodeError(err)
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

func (*Query[T]) checkRegistration() error { return requireStructT[T]("Query[T]") }

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

// FilePart is a multipart file part extracted from a request.
//
// The underlying file stream is opened lazily by Open or Save. It can be
// consumed only once.
type FilePart struct {
	Name        string
	Filename    string
	ContentType string
	Size        int64

	header   *multipart.FileHeader
	consumed *atomic.Bool
}

// Open opens the uploaded file for streaming. The returned file must be closed.
// Open and Save share the same one-shot consumption guard.
func (p *FilePart) Open() (multipart.File, error) {
	if p == nil || p.header == nil {
		return nil, errors.New("mint: missing file part")
	}
	if p.consumed == nil || !p.consumed.CompareAndSwap(false, true) {
		return nil, errors.New("mint: file already consumed")
	}
	file, err := p.header.Open()
	if err != nil {
		return nil, fmt.Errorf("open uploaded file %q: %w", p.Filename, err)
	}
	return file, nil
}

// Save streams the uploaded file to path. It creates parent directories as
// needed and removes the target if copying or closing fails.
func (p *FilePart) Save(path string) (err error) {
	if path == "" {
		return errors.New("mint: save path is required")
	}
	target := filepath.Clean(path)

	in, err := p.Open()
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := in.Close(); err == nil && closeErr != nil {
			err = closeErr
		}
	}()

	if err = os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("create upload directory: %w", err)
	}

	out, err := os.Create(target)
	if err != nil {
		return fmt.Errorf("create upload target: %w", err)
	}

	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		err = fmt.Errorf("save uploaded file: %w", copyErr)
	} else if closeErr != nil {
		err = fmt.Errorf("close upload target: %w", closeErr)
	}
	if err != nil {
		if removeErr := os.Remove(target); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, removeErr)
		}
		return err
	}
	return nil
}

// Consumed reports whether Open or Save has consumed this file part.
func (p *FilePart) Consumed() bool {
	return p != nil && p.consumed != nil && p.consumed.Load()
}

// File extracts the first uploaded file from a multipart/form-data request.
// For name-based file binding, use FilePart fields inside Form[T].
type File struct {
	Value FilePart
}

func (*File) consumesParsedForm() {}

func (f *File) Extract(r *http.Request) error {
	if err := parseRequestForm(r); err != nil {
		return mapFormParseError(err)
	}

	part, ok := firstFilePart(r.MultipartForm)
	if !ok {
		return NewFileMissingError("")
	}
	f.Value = part
	return nil
}

// Open is a convenience wrapper around f.Value.Open.
func (f *File) Open() (multipart.File, error) { return f.Value.Open() }

// Save is a convenience wrapper around f.Value.Save.
func (f *File) Save(path string) error { return f.Value.Save(path) }

func newFilePart(name string, header *multipart.FileHeader) FilePart {
	return FilePart{
		Name:        name,
		Filename:    header.Filename,
		ContentType: header.Header.Get("Content-Type"),
		Size:        header.Size,
		header:      header,
		consumed:    &atomic.Bool{},
	}
}

func firstFilePart(form *multipart.Form) (FilePart, bool) {
	if form == nil || len(form.File) == 0 {
		return FilePart{}, false
	}
	names := make([]string, 0, len(form.File))
	for name := range form.File {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		headers := nonNilFileHeaders(form.File[name])
		if len(headers) > 0 {
			return newFilePart(name, headers[0]), true
		}
	}
	return FilePart{}, false
}

// Form[T] parses URL-encoded or multipart form data and decodes r.Form into T
// using the schema decoder. In multipart requests, FilePart fields are bound
// from file parts by their schema/form tag name.
type Form[T any] struct {
	Value T
}

func (*Form[T]) checkRegistration() error { return requireStructT[T]("Form[T]") }

func (*Form[T]) consumesParsedForm() {}

func (f *Form[T]) Extract(r *http.Request) error {
	if err := parseRequestForm(r); err != nil {
		return mapFormParseError(err)
	}

	val := reflect.ValueOf(&f.Value).Elem()
	target := getPointer(val)

	if err := schemaDecoder().Decode(target, r.Form); err != nil {
		return err
	}
	if err := bindMultipartFiles(target, r.MultipartForm); err != nil {
		return err
	}
	if err := validate(target); err != nil {
		return NewValidationError(err)
	}
	return nil
}

func parseRequestForm(r *http.Request) error {
	if isMultipartForm(r) {
		return r.ParseMultipartForm(multipartMemory())
	}
	return r.ParseForm()
}

func isMultipartForm(r *http.Request) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && strings.EqualFold(mediaType, "multipart/form-data")
}

func bindMultipartFiles(target any, form *multipart.Form) error {
	if form == nil || len(form.File) == 0 {
		return nil
	}

	v := reflect.ValueOf(target)
	if v.Kind() != reflect.Ptr || v.IsNil() {
		return nil
	}
	v = v.Elem()
	if v.Kind() != reflect.Struct {
		return nil
	}
	return bindFilesToStruct(v, form, 0)
}

// maxEmbedDepth bounds recursion through embedded structs; a pointer-embed
// cycle would otherwise loop forever.
const maxEmbedDepth = 16

func bindFilesToStruct(v reflect.Value, form *multipart.Form, depth int) error {
	if depth > maxEmbedDepth {
		return nil
	}
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		if !sf.IsExported() {
			continue
		}
		name, ok := formBindingName(sf)
		if !ok {
			continue
		}
		// Recurse into untagged embedded structs so promoted FilePart
		// fields bind, mirroring how the schema decoder promotes values.
		if sf.Anonymous && !hasExplicitFormName(sf) {
			fv := v.Field(i)
			if fv.Kind() == reflect.Pointer {
				if fv.IsNil() {
					if !fv.CanSet() || !typeHasFileParts(fv.Type().Elem(), depth+1) {
						continue
					}
					fv.Set(reflect.New(fv.Type().Elem()))
				}
				fv = fv.Elem()
			}
			if fv.Kind() == reflect.Struct {
				if err := bindFilesToStruct(fv, form, depth+1); err != nil {
					return err
				}
				continue
			}
		}
		if err := setFileField(v.Field(i), name, form.File[name]); err != nil {
			return err
		}
	}
	return nil
}

func hasExplicitFormName(field reflect.StructField) bool {
	for _, tag := range []string{"schema", "form"} {
		if name := strings.SplitN(field.Tag.Get(tag), ",", 2)[0]; name != "" && name != "-" {
			return true
		}
	}
	return false
}

// typeHasFileParts reports whether t (a struct type) contains any field a
// multipart file could bind to, directly or via embedded structs.
func typeHasFileParts(t reflect.Type, depth int) bool {
	if depth > maxEmbedDepth || t.Kind() != reflect.Struct {
		return false
	}
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		if !sf.IsExported() {
			continue
		}
		ft := sf.Type
		switch {
		case ft == filePartType, ft == filePartPtrType:
			return true
		case ft.Kind() == reflect.Slice && (ft.Elem() == filePartType || ft.Elem() == filePartPtrType):
			return true
		}
		if sf.Anonymous {
			et := ft
			if et.Kind() == reflect.Pointer {
				et = et.Elem()
			}
			if typeHasFileParts(et, depth+1) {
				return true
			}
		}
	}
	return false
}

func formBindingName(field reflect.StructField) (string, bool) {
	for _, tag := range []string{"schema", "form"} {
		name := strings.SplitN(field.Tag.Get(tag), ",", 2)[0]
		if name == "-" {
			return "", false
		}
		if name != "" {
			return name, true
		}
	}
	return field.Name, true
}

func setFileField(field reflect.Value, name string, headers []*multipart.FileHeader) error {
	headers = nonNilFileHeaders(headers)
	if len(headers) == 0 || !field.CanSet() {
		return nil
	}

	switch {
	case field.Type() == filePartType:
		field.Set(reflect.ValueOf(newFilePart(name, headers[0])))
	case field.Type() == filePartPtrType:
		part := newFilePart(name, headers[0])
		field.Set(reflect.ValueOf(&part))
	case field.Kind() == reflect.Slice && field.Type().Elem() == filePartType:
		slice := reflect.MakeSlice(field.Type(), len(headers), len(headers))
		for i, header := range headers {
			slice.Index(i).Set(reflect.ValueOf(newFilePart(name, header)))
		}
		field.Set(slice)
	case field.Kind() == reflect.Slice && field.Type().Elem() == filePartPtrType:
		slice := reflect.MakeSlice(field.Type(), len(headers), len(headers))
		for i, header := range headers {
			part := newFilePart(name, header)
			slice.Index(i).Set(reflect.ValueOf(&part))
		}
		field.Set(slice)
	}
	return nil
}

func nonNilFileHeaders(headers []*multipart.FileHeader) []*multipart.FileHeader {
	for i, header := range headers {
		if header == nil {
			filtered := make([]*multipart.FileHeader, 0, len(headers)-1)
			filtered = append(filtered, headers[:i]...)
			for _, h := range headers[i+1:] {
				if h != nil {
					filtered = append(filtered, h)
				}
			}
			return filtered
		}
	}
	return headers
}

// Path[T] extracts a single path parameter from r.PathValue. The binding name
// is set by H() based on positional order of Path[T] parameters in the
// handler signature; see README "Pitfalls" for the implications.
type Path[T PathValue] struct {
	Value T

	key      string
	wildcard bool
}

// SetKey is invoked by the framework. Manually calling it before H() invokes
// the extractor (e.g., in unit tests for an extractor in isolation) is fine.
func (p *Path[T]) SetKey(key string) { p.key = key }

// Key returns the path-parameter name this extractor is bound to.
// Useful for debugging and custom error reporting.
func (p *Path[T]) Key() string { return p.key }

// setWildcard is invoked by the framework when the bound placeholder is a
// {name...} wildcard, whose empty match is legitimate.
func (p *Path[T]) setWildcard() { p.wildcard = true }

func (p *Path[T]) Extract(r *http.Request) error {
	pv := r.PathValue(p.key)
	if pv == "" {
		// "/files/" matched against "/files/{name...}" is a valid, empty
		// remainder — but only a string can represent it faithfully. For
		// non-string T the zero value would be indistinguishable from a
		// real "0"/"false" segment, so an empty match stays an error.
		if p.wildcard && reflect.ValueOf(p.Value).Kind() == reflect.String {
			return nil
		}
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

func (*Header[T]) checkRegistration() error { return requireStructT[T]("Header[T]") }

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

func (*Cookie[T]) checkRegistration() error { return requireStructT[T]("Cookie[T]") }

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

// NewJSONDecodeError wraps a JSON unmarshaling failure so it maps to 400 even
// when a custom JSONUnmarshalFunc returns unrecognized error types.
func NewJSONDecodeError(err error) error {
	return &ExtractError{
		Type:    ErrTypeJSONDecode,
		Message: "invalid JSON body",
		Err:     err,
	}
}

func NewFormParseError(err error) error {
	return &ExtractError{
		Type:    ErrTypeFormParse,
		Message: "invalid form data format",
		Err:     err,
	}
}

func NewFileMissingError(field string) error {
	msg := "missing uploaded file"
	if field != "" {
		msg = fmt.Sprintf("missing uploaded file: %s", field)
	}
	return &ExtractError{
		Type:    ErrTypeFileMissing,
		Field:   field,
		Message: msg,
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

// mapFormParseError converts a form/multipart parsing failure into the most
// specific ExtractError: size overflows (the MaxBytesReader cap or the
// multipart memory budget) become 413, everything else a 400 parse error.
func mapFormParseError(err error) error {
	if err == nil {
		return nil
	}
	var be *http.MaxBytesError
	if errors.As(err, &be) {
		return NewBodyTooLargeError(err)
	}
	if errors.Is(err, multipart.ErrMessageTooLarge) {
		return &ExtractError{
			Type:    ErrTypeBodyTooLarge,
			Message: "multipart form data too large",
			Err:     err,
		}
	}
	return NewFormParseError(err)
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
	} else if code < 100 || code > 999 {
		// net/http panics on out-of-range codes; fail the request instead.
		logger().Printf("mint: invalid status code %d; writing 500", code)
		code = http.StatusInternalServerError
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
		// The first flush on a live connection sends the headers with an
		// implicit 200; record that so later WriteHeader calls are treated
		// as the duplicates they are.
		if !rw.headerWritten {
			rw.headerWritten = true
			rw.statusCode = http.StatusOK
		}
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
	contextType        = reflect.TypeOf((*context.Context)(nil)).Elem()
	filePartType       = reflect.TypeOf(FilePart{})
	filePartPtrType    = reflect.TypeOf((*FilePart)(nil))
	timeType           = reflect.TypeOf(time.Time{})

	keySetterType          = reflect.TypeOf((*KeySetter)(nil)).Elem()
	rawBodyConsumerType    = reflect.TypeOf((*rawBodyConsumer)(nil)).Elem()
	parsedFormConsumerType = reflect.TypeOf((*parsedFormConsumer)(nil)).Elem()

	// responseWriterConcrete is the wrapper handed to handlers; custom
	// writer interfaces must be satisfiable by it.
	responseWriterConcrete = reflect.TypeOf((*ResponseWriter)(nil))
)

// paramKind classifies a handler parameter; decided once at registration.
type paramKind uint8

const (
	paramKindExtractor paramKind = iota
	paramKindResponseWriter
	paramKindRequest
	paramKindContext
)

// paramPlan is the per-parameter dispatch decision, computed by H() at
// registration so the request hot path does no interface probing.
type paramPlan struct {
	typ       reflect.Type
	kind      paramKind
	keySetter bool
}

func buildParamPlan(pt reflect.Type) (paramPlan, error) {
	switch {
	case isExtractorParam(pt):
		if checker, ok := reflect.New(pt).Interface().(registrationChecker); ok {
			if err := checker.checkRegistration(); err != nil {
				return paramPlan{}, err
			}
		}
		return paramPlan{
			typ:       pt,
			kind:      paramKindExtractor,
			keySetter: reflect.PointerTo(pt).Implements(keySetterType),
		}, nil

	case pt == contextType:
		return paramPlan{typ: pt, kind: paramKindContext}, nil

	case pt == httpRequestType:
		return paramPlan{typ: pt, kind: paramKindRequest}, nil

	case pt.Kind() == reflect.Interface && pt.Implements(responseWriterType):
		if !responseWriterConcrete.Implements(pt) {
			return paramPlan{}, fmt.Errorf(
				"writer interface %s cannot be satisfied by *mint.ResponseWriter (it only adds Flush/Hijack/Push/Unwrap/Status/Written)", pt)
		}
		return paramPlan{typ: pt, kind: paramKindResponseWriter}, nil

	default:
		return paramPlan{}, fmt.Errorf("unsupported parameter type %s", pt)
	}
}

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

	// Everything about the signature is decided once, here: per-parameter
	// dispatch plans (the hot path does no interface probing), extractor
	// type-parameter checks, and body-consumption conflicts.
	plans := make([]paramPlan, fnType.NumIn())
	needsPathKeys := false
	numRawBody, numParsedForm := 0, 0
	for i := 0; i < fnType.NumIn(); i++ {
		plan, err := buildParamPlan(fnType.In(i))
		if err != nil {
			panic(fmt.Sprintf("mint.H: %v at position %d", err, i))
		}
		plans[i] = plan
		if plan.keySetter {
			needsPathKeys = true
		}
		if plan.kind == paramKindExtractor {
			ptr := reflect.PointerTo(plan.typ)
			if ptr.Implements(rawBodyConsumerType) {
				numRawBody++
			}
			if ptr.Implements(parsedFormConsumerType) {
				numParsedForm++
			}
		}
	}
	if numRawBody > 1 {
		panic("mint.H: multiple JSON extractors in one handler; the request body can be read only once")
	}
	if numRawBody >= 1 && numParsedForm >= 1 {
		panic("mint.H: JSON and Form/File extractors in one handler would both consume the request body")
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

		// Enforce the body size cap once per request, before any extractor
		// reads. Pass the ORIGINAL writer: on overflow net/http recognizes
		// its own response type (via an unexported interface) to stop
		// reading and close the connection; the wrapper would hide it.
		if maxN := maxBodySize(); maxN > 0 {
			r.Body = http.MaxBytesReader(w, r.Body, maxN)
		}

		args := make([]reflect.Value, len(plans))
		var pathKeys []pathKey
		if needsPathKeys {
			pathKeys = patternKeys(r.Pattern)
		}
		keyIdx := 0

		for i := range plans {
			plan := &plans[i]
			switch plan.kind {
			case paramKindExtractor:
				paramVal := reflect.New(plan.typ).Elem()
				extractor := paramVal.Addr().Interface().(Extractor)

				if plan.keySetter {
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
					key := pathKeys[keyIdx]
					extractor.(KeySetter).SetKey(key.name)
					if key.wildcard {
						if ws, ok := extractor.(wildcardSetter); ok {
							ws.setWildcard()
						}
					}
					keyIdx++
				}

				if err := extractor.Extract(r); !isNilError(err) {
					if e := handleError(rw, err); e != nil {
						logger().Printf("mint: failed to write error response: %v", e)
					}
					return
				}
				args[i] = paramVal

			case paramKindResponseWriter:
				args[i] = reflect.ValueOf(rw)

			case paramKindRequest:
				args[i] = reflect.ValueOf(r)

			case paramKindContext:
				args[i] = reflect.ValueOf(r.Context())
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
				writeInternalErrorFallback(rw)
			}
		case 2:
			// A typed-nil error (e.g. `var e *HTTPError; return data, e`)
			// must count as "no error", so check through the interface.
			if !errorIsNil(results[1]) {
				if err := handleError(rw, results[1].Interface().(error)); err != nil {
					logger().Printf("mint: failed to write response: %v", err)
					writeInternalErrorFallback(rw)
				}
				return
			}
			if isNilValue(results[0]) {
				return
			}
			rv := results[0].Interface()
			if handler, ok := rv.(http.Handler); ok {
				handler.ServeHTTP(rw, r)
				return
			}
			if err := handleCommonTypes(rw, rv); err != nil {
				logger().Printf("mint: failed to write response: %v", err)
				writeInternalErrorFallback(rw)
			}
		}
	}
}

func isExtractorParam(paramType reflect.Type) bool {
	return reflect.PointerTo(paramType).Implements(extractorType)
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

// errorIsNil reports whether an error return slot holds no usable error:
// a nil interface, a nil concrete pointer, or a typed-nil wrapped inside the
// error interface (the classic `var e *MyErr; return data, e` trap).
func errorIsNil(v reflect.Value) bool {
	if !v.IsValid() {
		return true
	}
	if v.Kind() == reflect.Interface {
		if v.IsNil() {
			return true
		}
		v = v.Elem()
	}
	return isNilValue(v)
}

// isNilError is errorIsNil for plain error values.
func isNilError(err error) bool {
	if err == nil {
		return true
	}
	return errorIsNil(reflect.ValueOf(err))
}

// writeInternalErrorFallback emits a minimal 500 when nothing has been sent
// yet, so a failed response write doesn't surface as an empty 200.
func writeInternalErrorFallback(rw *ResponseWriter) {
	if rw.Written() {
		return
	}
	rw.Header().Set("Content-Type", "application/json; charset=utf-8")
	rw.WriteHeader(http.StatusInternalServerError)
	_, _ = io.WriteString(rw, `{"code":500,"error":"internal_error"}`)
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
		// Close reader-owned resources (e.g. *os.File) once streamed;
		// otherwise every request leaks a file descriptor.
		if c, ok := v.(io.Closer); ok {
			defer c.Close()
		}
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
	if !isNilError(result.Err) {
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
	if isNilError(err) {
		return nil
	}
	if eh := errorHandler(); eh != nil {
		eh(w, err)
		return nil
	}

	statusWritten := false
	if rw, ok := w.(*ResponseWriter); ok {
		statusWritten = rw.headerWritten
	}

	httpErr := toHTTPErrorWithCode(err, codeOverride)
	if httpErr == nil {
		return nil
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
	return toHTTPErrorWithCode(err, 0)
}

// toHTTPErrorWithCode is toHTTPError with an optional status override
// (Result.Code). The visibility policy is applied against the FINAL status
// code, not the inferred one. An explicit HTTPError is respected verbatim —
// the override only replaces its status code.
func toHTTPErrorWithCode(err error, codeOverride int) *HTTPError {
	if err == nil {
		return nil
	}

	// 1. Explicit HTTPError wins: Err and Message are kept regardless of code.
	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		if httpErr == nil {
			return nil
		}
		if codeOverride != 0 && codeOverride != httpErr.Code {
			clone := *httpErr
			clone.Code = codeOverride
			return &clone
		}
		return httpErr
	}
	var httpErrVal HTTPError
	if errors.As(err, &httpErrVal) {
		if codeOverride != 0 {
			httpErrVal.Code = codeOverride
		}
		return &httpErrVal
	}

	out, generic := implicitHTTPError(err)
	if codeOverride != 0 {
		out.Code = codeOverride
		if generic {
			// The tag was derived from the inferred code; re-derive it from
			// the code the handler actually asked for.
			out.Err = inferErrorType(codeOverride)
		}
	}
	// Visibility policy against the final code: clients may see what they
	// did wrong (4xx); internals stay hidden (5xx, logged by the caller).
	if out.Code >= 500 {
		out.Message = ""
	} else if generic && out.Message == "" {
		out.Message = err.Error()
	}
	return out
}

// implicitHTTPError maps errors that are not HTTPError. The generic result
// reports whether the mapping came from the message heuristics rather than a
// known error type (whose curated Message and tag should be preserved).
func implicitHTTPError(err error) (out *HTTPError, generic bool) {
	// ExtractError → fixed mapping per Type.
	var extractErr *ExtractError
	if errors.As(err, &extractErr) {
		return extractErrorToHTTP(extractErr), false
	}

	// Well-known stdlib / dependency error types.
	if h := wellKnownErrorToHTTP(err); h != nil {
		return h, false
	}

	// Last resort: infer the status from the error message.
	code := inferStatusCode(err.Error())
	return &HTTPError{Code: code, Err: inferErrorType(code)}, true
}

// wellKnownErrorToHTTP maps well-known stdlib / dependency error types.
// errors.As is used throughout so wrapped errors are recognized too.
func wellKnownErrorToHTTP(err error) *HTTPError {
	if err == nil {
		return nil
	}

	var maxBytesErr *http.MaxBytesError
	if errors.As(err, &maxBytesErr) {
		return &HTTPError{
			Code:    http.StatusRequestEntityTooLarge,
			Err:     "body_too_large",
			Message: fmt.Sprintf("request body exceeds %d bytes", maxBytesErr.Limit),
		}
	}
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		return &HTTPError{
			Code:    http.StatusBadRequest,
			Err:     "invalid_json_type",
			Message: fmt.Sprintf("field %q expects %s but got %s", typeErr.Field, typeErr.Type.String(), typeErr.Value),
		}
	}
	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) {
		return &HTTPError{
			Code:    http.StatusBadRequest,
			Err:     "invalid_json_syntax",
			Message: "invalid JSON syntax",
		}
	}
	var multiErr schema.MultiError
	if errors.As(err, &multiErr) {
		msgs := make([]string, 0, len(multiErr))
		for field, fieldErr := range multiErr {
			msgs = append(msgs, fmt.Sprintf("%s: %s", field, fieldErr.Error()))
		}
		sort.Strings(msgs) // map order is random; keep responses deterministic
		return &HTTPError{
			Code:    http.StatusBadRequest,
			Err:     "validation_failed",
			Message: strings.Join(msgs, "; "),
		}
	}
	var convErr *schema.ConversionError
	if errors.As(err, &convErr) {
		return &HTTPError{
			Code:    http.StatusBadRequest,
			Err:     "conversion_failed",
			Message: fmt.Sprintf("cannot convert field %q", convErr.Key),
		}
	}
	var unknownKeyErr *schema.UnknownKeyError
	if errors.As(err, &unknownKeyErr) {
		return &HTTPError{
			Code:    http.StatusBadRequest,
			Err:     "unknown_field",
			Message: fmt.Sprintf("unknown field: %s", unknownKeyErr.Key),
		}
	}
	return nil
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
	case ErrTypeJSONDecode:
		// Keep the detailed stdlib mappings (invalid_json_syntax/_type) when
		// recognizable; otherwise report a generic decode failure.
		if h := wellKnownErrorToHTTP(e.Err); h != nil {
			return h
		}
		return &HTTPError{Code: http.StatusBadRequest, Err: "json_decode_error", Message: e.Message}
	case ErrTypeFormParse:
		return &HTTPError{Code: http.StatusBadRequest, Err: "invalid_form", Message: e.Message}
	case ErrTypeFileMissing:
		return &HTTPError{Code: http.StatusBadRequest, Err: "missing_file", Message: e.Message}
	case ErrTypePathConversion:
		return &HTTPError{Code: http.StatusBadRequest, Err: "invalid_path_parameter", Message: e.Message}
	case ErrTypeMissingPath:
		return &HTTPError{Code: http.StatusBadRequest, Err: "missing_path_parameter", Message: e.Message}
	case ErrTypeValidation:
		return &HTTPError{Code: http.StatusBadRequest, Err: "validation_failed", Message: e.Message}
	case "unsupported_type":
		// A handler/extractor type problem is the server's fault, not the
		// client's; the message is logged via the 5xx path, not echoed.
		return &HTTPError{Code: http.StatusInternalServerError, Err: "internal_server_error"}
	default:
		return &HTTPError{Code: http.StatusBadRequest, Err: e.Type, Message: e.Message}
	}
}

func inferStatusCode(msg string) int {
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "not found"),
		strings.Contains(lower, "does not exist"),
		strings.Contains(lower, "doesn't exist"):
		return http.StatusNotFound
	case strings.Contains(lower, "unauthorized"):
		return http.StatusUnauthorized
	case strings.Contains(lower, "forbidden"):
		return http.StatusForbidden
	case strings.Contains(lower, "timeout"):
		return http.StatusRequestTimeout
	case strings.Contains(lower, "conflict"),
		strings.Contains(lower, "already exists"):
		return http.StatusConflict
	case strings.Contains(lower, "bad request"),
		strings.Contains(lower, "invalid"),
		strings.Contains(lower, "validation"):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

// inferErrorType returns a snake_case error tag for a status code. A few
// historical tags are pinned; everything else derives from http.StatusText
// (409 → "conflict", 422 → "unprocessable_entity", …).
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
	case http.StatusInternalServerError:
		return "internal_error"
	}
	if code >= 400 {
		if text := http.StatusText(code); text != "" {
			return statusTextTag(text)
		}
		if code < 500 {
			return "bad_request"
		}
	}
	return "internal_error"
}

// statusTextTag converts an http.StatusText value ("Unprocessable Entity")
// into a snake_case tag ("unprocessable_entity").
func statusTextTag(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	for _, r := range strings.ToLower(text) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '-':
			b.WriteByte('_')
		}
	}
	return b.String()
}

// =============================================================================
// Pattern parsing
// =============================================================================

// pathKey is one {name} placeholder parsed from a route pattern.
type pathKey struct {
	name     string
	wildcard bool // true for {name...}
}

var (
	patternCache   sync.Map // map[string][]pathKey
	patternWarnMu  sync.Mutex
	patternWarnSet = map[string]struct{}{}
)

// patternKeys returns the path-parameter keys from a Go 1.22+ pattern. The
// result is cached because the same pattern repeats every request.
func patternKeys(pattern string) []pathKey {
	if v, ok := patternCache.Load(pattern); ok {
		return v.([]pathKey)
	}
	keys := extractPatternNames(pattern)
	patternCache.Store(pattern, keys)
	return keys
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

// extractPatternNames parses a pattern like "GET /users/{id}/posts/{p...}"
// and returns its placeholder keys: [{id false}, {p true}]. The "{$}"
// end-of-path marker is skipped. Unbalanced braces emit a logger warning.
func extractPatternNames(pattern string) []pathKey {
	keys := []pathKey{}
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
				keys = append(keys, pathKey{name: name, wildcard: name != current})
			}
		case inParam:
			current += string(ch)
		}
	}
	if depth != 0 {
		logger().Printf("mint: unbalanced braces in pattern %q", pattern)
	}
	return keys
}

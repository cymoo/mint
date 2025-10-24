package m

import (
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"

	"github.com/go-playground/validator/v10"
	"github.com/gorilla/schema"
)

// Config holds global configuration for the framework
type Config struct {
	// SchemaDecoder for parsing form and query parameters
	// If nil, a default decoder will be created
	SchemaDecoder *schema.Decoder

	// JSONMarshalFunc for encoding JSON responses
	// If nil, json.Marshal will be used
	JSONMarshalFunc func(v any) ([]byte, error)

	// JSONEncodeFunc for streaming JSON encoding
	// If nil, json.NewEncoder(w).Encode(v) will be used
	JSONEncodeFunc func(w io.Writer, v any) error

	// JSONUnmarshalFunc for decoding JSON requests
	// If nil, json.Unmarshal will be used
	JSONUnmarshalFunc func(data []byte, v any) error

	// Logger allows user to provide custom logger. If nil, log.Default() is used.
	Logger *log.Logger

	// EnableValidation enables automatic validation for JSON, Query, and Form extractors
	// Default: true
	EnableValidation bool

	// Validator is the validation instance to use
	// If nil and EnableValidation is true, a default validator will be created
	Validator *validator.Validate
}

var (
	configMu           sync.RWMutex
	config             *Config
	configOnce         sync.Once
	CustomErrorHandler func(w http.ResponseWriter, err error)
)

func initDefaultConfig() {
	configOnce.Do(func() {
		if config == nil {
			config = &Config{
				EnableValidation: true,
				Validator:        newDefaultValidator(),
			}
		}
	})
}

func newDefaultValidator() *validator.Validate {
	v := validator.New()
	// Use json tag as field name for validation errors
	v.RegisterTagNameFunc(func(fld reflect.StructField) string {
		name := strings.SplitN(fld.Tag.Get("json"), ",", 2)[0]
		if name == "-" {
			return ""
		}
		if name != "" {
			return name
		}
		// Fallback to form tag
		name = strings.SplitN(fld.Tag.Get("form"), ",", 2)[0]
		if name == "-" {
			return ""
		}
		return name
	})
	return v
}

func SetConfig(cfg *Config) {
	configMu.Lock()
	defer configMu.Unlock()
	if cfg == nil {
		initDefaultConfig()
		return
	}
	if cfg.EnableValidation && cfg.Validator == nil {
		cfg.Validator = newDefaultValidator()
	}
	config = cfg
}

func getConfig() *Config {
	initDefaultConfig()
	configMu.RLock()
	defer configMu.RUnlock()
	return config
}

// logger returns the configured logger or the default logger.
func (c *Config) logger() *log.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return log.Default()
}

func (c *Config) schemaDecoder() *schema.Decoder {
	if c.SchemaDecoder == nil {
		decoder := schema.NewDecoder()
		decoder.IgnoreUnknownKeys(true)
		return decoder
	}
	return c.SchemaDecoder
}

func (c *Config) jsonEncode(w io.Writer, v any) error {
	if c.JSONEncodeFunc != nil {
		return c.JSONEncodeFunc(w, v)
	}

	if c.JSONMarshalFunc != nil {
		jsonData, err := c.JSONMarshalFunc(v)
		if err != nil {
			return err
		}
		_, err = w.Write(jsonData)
		return err
	}

	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(v)
}

func (c *Config) jsonUnmarshal(data []byte, v any) error {
	if c.JSONUnmarshalFunc == nil {
		return json.Unmarshal(data, v)
	}
	return c.JSONUnmarshalFunc(data, v)
}

func (c *Config) validate(v any) error {
	if !c.EnableValidation || c.Validator == nil {
		return nil
	}
	return c.Validator.Struct(v)
}

const (
	ErrTypeBodyRead       = "body_read_error"
	ErrTypeEmptyBody      = "empty_body"
	ErrTypeFormParse      = "form_parse_error"
	ErrTypePathConversion = "path_conversion_error"
	ErrTypeMissingPath    = "missing_path_value"
	ErrTypeValidation     = "validation_error"
)

var (
	extractorType = reflect.TypeOf((*Extractor)(nil)).Elem()
	errorType     = reflect.TypeOf((*error)(nil)).Elem()
	readerType    = reflect.TypeOf((*io.Reader)(nil)).Elem()

	handlerType        = reflect.TypeOf((*http.Handler)(nil)).Elem()
	responseWriterType = reflect.TypeOf((*http.ResponseWriter)(nil)).Elem()
	httpRequestType    = reflect.TypeOf((*http.Request)(nil))
)

type StatusCode int
type HTML string

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

type Result[T any] struct {
	Code    int
	Headers http.Header

	Data T
	Err  error
}

func (Result[T]) isResultType() bool {
	return true
}

func (r Result[T]) toResult() Result[any] {
	return Result[any]{
		Code:    r.Code,
		Headers: r.Headers,
		Data:    r.Data,
		Err:     r.Err,
	}
}

func OK[T any](data T) Result[T] {
	return Result[T]{Data: data}
}

func Err[T any](code int, err error) Result[T] {
	return Result[T]{
		Code: code,
		Err:  err,
	}
}

type Extractor interface {
	Extract(*http.Request) error
}

type KeySetter interface {
	SetKey(string)
}

type Responder interface {
	Respond(w http.ResponseWriter)
}

type PathValue interface {
	~string | ~int | ~int64 | ~uint | ~uint64 | ~float64 | ~bool
}

type JSON[T any] struct {
	Value T
}

func (j *JSON[T]) Extract(r *http.Request) error {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return NewBodyReadError(err)
	}

	if len(body) == 0 {
		return NewEmptyBodyError()
	}

	val := reflect.ValueOf(&j.Value).Elem()

	target := getPointer(val)

	if err := getConfig().jsonUnmarshal(body, target); err != nil {
		return err
	}

	if err := getConfig().validate(target); err != nil {
		return NewValidationError(err)
	}

	return nil
}

type Query[T any] struct {
	Value T
}

func (q *Query[T]) Extract(r *http.Request) error {
	val := reflect.ValueOf(&q.Value).Elem()

	target := getPointer(val)
	if err := getConfig().schemaDecoder().Decode(target, r.URL.Query()); err != nil {
		return err
	}

	if err := getConfig().validate(target); err != nil {
		return NewValidationError(err)
	}

	return nil
}

type Form[T any] struct {
	Value T
}

func (f *Form[T]) Extract(r *http.Request) error {
	if err := r.ParseForm(); err != nil {
		return NewFormParseError(err)
	}

	val := reflect.ValueOf(&f.Value).Elem()
	target := getPointer(val)
	if err := getConfig().schemaDecoder().Decode(target, r.Form); err != nil {
		return err
	}

	if err := getConfig().validate(target); err != nil {
		return NewValidationError(err)
	}

	return nil
}

type Path[T PathValue] struct {
	Value T
	Key   string
}

func (p *Path[T]) SetKey(key string) {
	p.Key = key
}

func (p *Path[T]) Extract(r *http.Request) error {
	pv := r.PathValue(p.Key)
	if pv == "" {
		return NewMissingPathError(p.Key)
	}

	switch ptr := any(&p.Value).(type) {
	case *string:
		*ptr = pv
	case *int:
		if val, err := strconv.Atoi(pv); err != nil {
			return NewPathConversionError(p.Key, pv, "int", err)
		} else {
			*ptr = val
		}
	case *int64:
		if val, err := strconv.ParseInt(pv, 10, 64); err != nil {
			return NewPathConversionError(p.Key, pv, "int64", err)
		} else {
			*ptr = val
		}
	case *uint:
		if val, err := strconv.ParseUint(pv, 10, 0); err != nil {
			return NewPathConversionError(p.Key, pv, "uint", err)
		} else {
			*ptr = uint(val)
		}
	case *uint64:
		if val, err := strconv.ParseUint(pv, 10, 64); err != nil {
			return NewPathConversionError(p.Key, pv, "uint64", err)
		} else {
			*ptr = val
		}
	case *float64:
		if val, err := strconv.ParseFloat(pv, 64); err != nil {
			return NewPathConversionError(p.Key, pv, "float64", err)
		} else {
			*ptr = val
		}
	case *bool:
		if val, err := strconv.ParseBool(pv); err != nil {
			return NewPathConversionError(p.Key, pv, "bool", err)
		} else {
			*ptr = val
		}
	default:
		return &ExtractError{
			Type:    "unsupported_type",
			Field:   p.Key,
			Message: fmt.Sprintf("Unsupported path parameter type: %T", &p.Value),
		}
	}
	return nil
}

type ExtractError struct {
	Type    string
	Field   string
	Value   string
	Message string
	Err     error
}

func (e *ExtractError) Error() string {
	return e.Message
}

func (e *ExtractError) Unwrap() error {
	return e.Err
}

type ResponseWriter struct {
	http.ResponseWriter
	statusCode    int
	headerWritten bool
}

func (rw *ResponseWriter) WriteHeader(code int) {
	if rw.headerWritten {
		getConfig().logger().Printf("Warning: multiple calls to WriteHeader, original status code: %d, new status code: %d", rw.statusCode, code)
		return
	}
	if code <= 0 {
		code = 200
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

type resultMarker interface {
	isResultType() bool
	toResult() Result[any]
}

func H(fn any) http.HandlerFunc {
	fnVal := reflect.ValueOf(fn)
	fnType := fnVal.Type()

	if fnType.Kind() != reflect.Func {
		log.Panicf("H: handler must be a function, got %T", fn)
	}

	paramTypes := make([]reflect.Type, fnType.NumIn())
	for i := 0; i < fnType.NumIn(); i++ {
		paramTypes[i] = fnType.In(i)
	}

	numOut := fnType.NumOut()
	if numOut > 2 {
		log.Panicf("H: handler can return at most 2 values, got %d", numOut)
	}

	if numOut == 1 {
		rt := fnType.Out(0)
		if rt.Kind() == reflect.Interface {
			if !rt.Implements(errorType) && !rt.Implements(handlerType) && !rt.Implements(readerType) {
				log.Panic("H: interface return type must implement error, http.Handler or io.Reader")
			}
		}
	}

	if numOut == 2 {
		rt1 := fnType.Out(0)
		rt2 := fnType.Out(1)

		if rt1.Kind() == reflect.Interface {
			log.Panic("H: first return value cannot be an interface when returning two values")
		}
		if rt1.Implements(reflect.TypeOf((*resultMarker)(nil)).Elem()) {
			log.Panicf("H: first return value cannot be Result when returning two values")
		}

		if !rt2.Implements(errorType) {
			log.Panicf("H: second return value must be error, got %s", rt2.String())
		}
	}

	return func(w http.ResponseWriter, r *http.Request) {
		args := make([]reflect.Value, len(paramTypes))

		pathKeys := extractPatternNames(r.Pattern)
		keyIdx := 0

		rw := &ResponseWriter{ResponseWriter: w}

		for i, paramType := range paramTypes {
			switch {
			case reflect.PointerTo(paramType).Implements(extractorType):
				paramVal := reflect.New(paramType).Elem()
				extractor := paramVal.Addr().Interface().(Extractor)

				if ks, ok := extractor.(KeySetter); ok {
					if keyIdx >= len(pathKeys) {
						log.Panicf("H: pattern %q has insufficient path parameters", r.Pattern)
					}
					ks.SetKey(pathKeys[keyIdx])
					keyIdx++
				}

				if err := extractor.Extract(r); err != nil {
					e := handleError(rw, err)
					if e != nil {
						getConfig().logger().Printf("failed to write error response: %v", e)
					}
					return
				}
				args[i] = paramVal

			case paramType.Implements(responseWriterType) && paramType.Kind() == reflect.Interface:
				args[i] = reflect.ValueOf(rw)

			case paramType == httpRequestType:
				args[i] = reflect.ValueOf(r)

			default:
				log.Panicf("H: unsupported parameter type %s", paramType.String())
			}
		}

		results := fnVal.Call(args)

		if len(results) == 0 {
			return
		}

		if len(results) == 1 {
			if isNilValue(results[0]) {
				return
			}

			rv := results[0].Interface()
			if handler, ok := rv.(http.Handler); ok {
				handler.ServeHTTP(rw, r)
				return
			}

			err := handleOneResult(rw, rv)
			if err != nil {
				getConfig().logger().Printf("failed to write response: %v", err)
			}
		}

		if len(results) == 2 {
			if isNilValue(results[0]) && isNilValue(results[1]) {
				return
			}

			rv := results[0].Interface()
			err := results[1].Interface()

			e := handleTwoResults(rw, rv, err)
			if e != nil {
				getConfig().logger().Printf("failed to write response: %v", e)
			}
		}
	}
}

func WriteHeaders(w http.ResponseWriter, headers http.Header) {
	for key, values := range headers {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
}

func NewBodyReadError(err error) error {
	return &ExtractError{
		Type:    ErrTypeBodyRead,
		Message: "failed to read request body",
		Err:     err,
	}
}

func NewEmptyBodyError() error {
	return &ExtractError{
		Type:    ErrTypeEmptyBody,
		Message: "request body is required",
	}
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

// formatValidationError formats validation errors into user-friendly messages
func formatValidationError(err error) string {
	var ve validator.ValidationErrors
	if !errors.As(err, &ve) {
		return err.Error()
	}

	if len(ve) == 0 {
		return "validation failed"
	}

	messages := make([]string, 0, len(ve))
	for _, fe := range ve {
		field := fe.Field()
		if field == "" {
			field = fe.StructField()
		}

		msg := formatFieldError(field, fe)
		messages = append(messages, msg)
	}

	return strings.Join(messages, "; ")
}

// formatFieldError formats a single field validation error
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
			elemType := val.Type().Elem()
			newVal := reflect.New(elemType)
			val.Set(newVal)
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
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, err := fmt.Fprint(w, v)
		return err
	case StatusCode:
		w.WriteHeader(int(v))
		return nil
	case []byte:
		w.Header().Set("Content-Type", "application/octet-stream")
		_, err := w.Write(v)
		return err
	case HTML, template.HTML:
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, err := fmt.Fprint(w, v)
		return err
	case io.Reader:
		_, err := io.Copy(w, v)
		return err
	default:
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		return config.jsonEncode(w, data)
	}
}

func handleResult(w http.ResponseWriter, result Result[any]) error {
	if result.Headers != nil {
		WriteHeaders(w, result.Headers)
	}

	if result.Code != 0 {
		w.WriteHeader(result.Code)
	}

	if result.Err != nil {
		return handleError(w, result.Err)
	}

	return handleCommonTypes(w, result.Data)
}

func handleError(w http.ResponseWriter, err error) error {
	if CustomErrorHandler != nil {
		CustomErrorHandler(w, err)
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

	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	if !statusWritten {
		w.WriteHeader(httpErr.Code)
	}

	return config.jsonEncode(w, httpErr)
}

func toHTTPError(err error) *HTTPError {
	if err == nil {
		return nil
	}

	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		return httpErr
	}

	var httpErrVal HTTPError
	if errors.As(err, &httpErrVal) {
		return &httpErrVal
	}

	var extractErr *ExtractError
	if errors.As(err, &extractErr) {
		switch extractErr.Type {
		case ErrTypeBodyRead:
			return &HTTPError{
				Code:    500,
				Err:     "internal_server_error",
				Message: "unable to process request",
			}
		case ErrTypeEmptyBody:
			return &HTTPError{
				Code:    400,
				Err:     "empty_body",
				Message: extractErr.Message,
			}
		case ErrTypeFormParse:
			return &HTTPError{
				Code:    400,
				Err:     "invalid_form",
				Message: extractErr.Message,
			}
		case ErrTypePathConversion:
			return &HTTPError{
				Code:    400,
				Err:     "invalid_path_parameter",
				Message: extractErr.Message,
			}
		case ErrTypeMissingPath:
			return &HTTPError{
				Code:    400,
				Err:     "missing_path_parameter",
				Message: extractErr.Message,
			}
		case ErrTypeValidation:
			return &HTTPError{
				Code:    400,
				Err:     "validation_failed",
				Message: extractErr.Message,
			}
		default:
			return &HTTPError{
				Code:    400,
				Err:     extractErr.Type,
				Message: extractErr.Message,
			}
		}
	}

	switch e := err.(type) {
	case *json.UnmarshalTypeError:
		return &HTTPError{
			Code:    400,
			Err:     "invalid_json_type",
			Message: fmt.Sprintf("field %q expects %s but got %s", e.Field, e.Type.String(), e.Value),
		}
	case *json.SyntaxError:
		return &HTTPError{
			Code:    400,
			Err:     "invalid_json_syntax",
			Message: "invalid JSON syntax",
		}

	case schema.MultiError:
		messages := make([]string, 0, len(e))
		for field, fieldErr := range e {
			messages = append(messages, fmt.Sprintf("%s: %s", field, fieldErr.Error()))
		}
		return &HTTPError{
			Code:    400,
			Err:     "validation_failed",
			Message: strings.Join(messages, "; "),
		}
	case *schema.ConversionError:
		return &HTTPError{
			Code:    400,
			Err:     "conversion_failed",
			Message: fmt.Sprintf("cannot convert field %q", e.Key),
		}
	case *schema.UnknownKeyError:
		return &HTTPError{
			Code:    400,
			Err:     "unknown_field",
			Message: fmt.Sprintf("unknown field: %s", e.Key),
		}

	default:
		errMsg := err.Error()
		code := inferStatusCode(errMsg)
		return &HTTPError{
			Code: code,
			Err:  inferErrorType(code),
		}
	}
}

func inferStatusCode(msg string) int {
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "not found"):
		return 404
	case strings.Contains(lower, "unauthorized"):
		return 401
	case strings.Contains(lower, "forbidden"):
		return 403
	case strings.Contains(lower, "timeout"):
		return 408
	case strings.Contains(lower, "bad request"), strings.Contains(lower, "invalid"):
		return 400
	default:
		return 500
	}
}

func inferErrorType(code int) string {
	switch code {
	case 400:
		return "bad_request"
	case 401:
		return "unauthorized"
	case 403:
		return "forbidden"
	case 404:
		return "not_found"
	case 408:
		return "timeout"
	default:
		return "internal_error"
	}
}

func extractPatternNames(pattern string) []string {
	var names []string
	inParam := false
	currentName := ""
	depth := 0

	for i, char := range pattern {
		if char == '{' {
			if inParam {
				getConfig().logger().Printf("warning: nested braces at position %d in pattern %q", i, pattern)
			}
			inParam = true
			depth++
			currentName = ""
		} else if char == '}' {
			if !inParam {
				getConfig().logger().Printf("warning: unmatched closing brace at position %d in pattern %q", i, pattern)
				continue
			}
			inParam = false
			depth--
			if currentName != "" {
				names = append(names, currentName)
			}
		} else if inParam {
			currentName += string(char)
		}
	}

	if depth != 0 {
		getConfig().logger().Printf("warning: unbalanced braces in pattern %q", pattern)
	}

	return names
}

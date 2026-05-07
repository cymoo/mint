# Copilot instructions for Mint

## Build, test, and run commands

- Full test suite: `go test ./...`
- Root package tests: `go test .`
- Single test: `go test -run '^TestJSONExtractor$' .`
- Single subtest: `go test -run 'TestJSONExtractor/valid_json' .`
- Run the demo server: `go run ./_examples`

## High-level architecture

Mint is a small Go module (`github.com/cymoo/mint`) whose public API lives in `mint.go` under package name `m`. It wraps standard `net/http`; applications still use `http.NewServeMux()` and register routes with `mux.HandleFunc(pattern, m.H(handler))`.

`H(fn any)` is the central adapter. At registration time it uses reflection to validate the handler signature and cache parameter types. At request time it builds arguments from supported inputs, calls the handler, and writes the returned value through the framework response pipeline.

Request data is injected through extractor types that implement `Extractor`:

- `JSON[T]` reads and unmarshals the request body, then validates the target.
- `Query[T]` decodes `r.URL.Query()` through the configured `gorilla/schema.Decoder`, then validates.
- `Form[T]` calls `r.ParseForm()`, decodes `r.Form`, then validates.
- `Path[T]` also implements `KeySetter`; `H` derives path variable names from `r.Pattern` and assigns them to `Path` parameters in handler parameter order before `Extract` reads `r.PathValue`.

Responses are normalized in `handleOneResult`, `handleTwoResults`, `handleCommonTypes`, and `handleResult`. Special return types include `StatusCode`, `HTML`, `[]byte`, `io.Reader`, `http.Handler`, `Responder`, `Result[T]`, `error`, and `(T, error)`; other values are JSON-encoded.

Configuration is global and thread-safe through `Initialize`, `Configure`, and `Reset`. Defaults include a schema decoder with `IgnoreUnknownKeys(true)`, validation enabled with `go-playground/validator`, `json.Marshal`/`json.Unmarshal`, and `log.Default()`. Tests that mutate configuration should call `Reset()`.

Error handling flows through `handleError` and `toHTTPError`. Prefer returning `HTTPError` for explicit HTTP status/error payloads and `ExtractError` from extractors for request extraction failures. Generic errors are converted by message heuristics such as "not found" -> 404.

## Key conventions

- Handler functions passed to `H` may return zero, one, or two values only. Two-value returns must be `(non-interface, error)`, and the first return value cannot be `Result[T]`.
- Handler parameters are limited to built-in/custom extractors, `http.ResponseWriter`, and `*http.Request`; unsupported parameter types panic during request handling.
- Custom extractors should define `Extract(*http.Request) error` on a pointer receiver so `reflect.PointerTo(paramType).Implements(extractorType)` detects them when handlers accept the non-pointer extractor value.
- Use `Result[T]`, `OK[T]`, and `Err[T]` when a handler needs custom status codes, headers, or a typed error result.
- Use `json` tags for JSON bodies and validation field names. Use `schema` tags for query/form decoding; the default validator also uses `schema` tags when choosing validation field names.
- Path parameters depend on Go 1.23 `net/http` route patterns and tests should set both `req.Pattern` and `req.SetPathValue(...)`; `createRequestWithPattern` in `mint_test.go` is the existing helper.
- Keep tests table/subtest-oriented with `testing`, `httptest`, and helpers like `parseJSONResponse`; test files are in package `m`, so they can exercise unexported helpers directly.
- The `_examples` directory is a runnable demo, but it is ignored by `go test ./...` because its name starts with an underscore.

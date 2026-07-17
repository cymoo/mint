# Changelog

## v1.2.0 — bugfixes, new features, performance

Eleven correctness fixes from a full source review, plus a feature round
and hot-path optimizations. Existing handler code keeps compiling; a few
previously-accepted **misconfigured** signatures now panic at registration
(see "New registration-time checks") and a few error responses change
shape as noted below.

### New features

- **`context.Context` handler parameters.** `H(func(ctx context.Context,
  id m.Path[int]) …)` injects `r.Context()` — no need to accept the whole
  `*http.Request` just for the context.
- **`WithMultipartMemory(n)`** configures the in-memory budget passed to
  `ParseMultipartForm` (previously hard-wired to 32 MiB).
- **Multipart file binding follows untagged embedded structs** (value and
  pointer embeds), mirroring how the schema decoder promotes value fields.
- **`{name...}` wildcard placeholders** bind to `Path[string]` and accept
  an empty remainder: `GET /files/` against `/files/{p...}` now yields
  `""` instead of `400 missing_path_parameter`. String bindings only —
  non-string `Path[T]` wildcards still 400 on an empty match, since the
  zero value would be indistinguishable from a real `0` segment.

### New registration-time checks (H panics at startup)

- `Query[T]`, `Form[T]`, `Header[T]`, `Cookie[T]` with a non-struct `T`
  (previously: every request failed at runtime, blamed on the client).
- Two `JSON[T]` parameters, or `JSON[T]` mixed with `Form[T]`/`File` —
  the body can only be consumed once (previously: confusing
  `500 body_read_error` per request). `Form` + `File` together stays
  valid.
- A custom writer interface parameter that `*mint.ResponseWriter` cannot
  satisfy (previously: registration passed, every request panicked).
- Relatedly, a request-time `unsupported_type` extractor error now maps
  to **500** instead of 400 — it is the server's bug, not the client's.

### Dependencies

- `go-playground/validator` v10.22.0 → v10.30.3;
  `golang.org/x/crypto` v0.19.0 → v0.54.0 (clears the CVE-2024-45337
  scanner finding; the vulnerable ssh path was never used),
  `golang.org/x/net` is no longer required at all. The module's Go
  directive moves to 1.25 (dependency floor).

### Performance

- `H()` now precomputes a per-parameter dispatch plan at registration;
  the request hot path does no `reflect.Implements` probing.
- Global config reads are lock-free (`atomic.Pointer[Config]` instead of
  an `RWMutex`), so requests never contend on configuration.
- The default JSON encoder recycles its buffers through a `sync.Pool`
  (buffers over 1 MiB are not retained).
- Route patterns are parsed into cached key lists only when the handler
  actually declares path parameters.

### Fixed

- **Non-struct JSON bodies no longer fail validation.** `JSON[[]T]`,
  `JSON[map[K]V]`, `JSON[int]`, `JSON[time.Time]`, … used to return
  `400 validation_failed` ("validator: (nil …)") for perfectly valid
  bodies whenever validation was enabled (the default). Validation now
  runs only for struct types; everything else decodes and skips it.
- **Typed-nil errors are treated as success.** A handler returning
  `(data, e)` where `e` is a nil `*HTTPError` (or any typed-nil error,
  directly or inside the `error` interface, including `Result.Err`)
  previously dropped the data — or crashed the request with a nil-pointer
  panic. The data is now written normally.
- **Message visibility follows the final status code.**
  `m.Err[T](400, errors.New("name is required"))` used to lose its message
  (the code was inferred as 500 first, hiding it); worse,
  `m.Err[T](500, errors.New("invalid …"))` **leaked** the internal message
  into the 5xx body. Both now follow the documented rule based on the code
  you actually asked for. An explicit `*HTTPError` keeps its `Err` tag and
  `Message` verbatim; `Result.Code` only overrides its status.
- **`io.Reader` returns are closed after streaming** when they implement
  `io.Closer` — returning an `*os.File` no longer leaks a file descriptor
  per request.
- **Out-of-range status codes no longer panic the request.**
  `StatusCode(1000)` or `Result{Code: 42}` are coerced to 500 and logged
  instead of hitting net/http's `WriteHeader` panic.
- **`Flush()` records that headers went out** (real connections send an
  implicit 200 on first flush), so an error returned after flushing no
  longer triggers a "superfluous WriteHeader" write.
- **Decode failures from a custom `WithJSONUnmarshal` function map to
  `400 json_decode_error`** instead of falling through to a hidden 500.
  Stdlib errors keep their `invalid_json_syntax` / `invalid_json_type`
  tags (now recognized even when wrapped, via `errors.As`).
- **`multipart.ErrMessageTooLarge` maps to 413** (`body_too_large`)
  instead of `400 invalid_form`.
- **A JSON-encoding failure now produces a 500** instead of an empty 200
  when nothing has been written yet.
- **`http.MaxBytesReader` receives the original `ResponseWriter`**, so
  net/http can flag the connection for closing after a 413 again.

### Changed

- `inferErrorType` derives tags for codes beyond the original six from
  `http.StatusText`: `409 → "conflict"`, `422 → "unprocessable_entity"`,
  `429 → "too_many_requests"`, `503 → "service_unavailable"` (previously
  all `"internal_error"`).
- Message-based status inference now also recognizes
  "does not exist"/"doesn't exist" (404), "conflict"/"already exists"
  (409), and "validation" (400), matching the README.
- `schema.MultiError` messages are sorted for deterministic responses.
- `PathValue` now admits every type the README documents:
  `int8`/`int16`/`int32`, `uint8`/`uint16`/`uint32`, `uintptr`,
  `float32` (the `Path[T]` conversion switch already handled them).

## v1.1.0 — multipart form file extraction

- `FilePart` multipart parts bind to `Form[T]` struct fields by
  `schema`/`form` tag (`FilePart`, `*FilePart`, `[]FilePart`,
  `[]*FilePart`).
- `File` extractor pulls the first uploaded file from a multipart
  request.
- `FilePart.Save`/`Open` stream the upload with a one-shot consumption
  guard.

## v1.0.0 — first stable release

This release is a comprehensive review pass that hardens the framework
for production use. It contains intentional **breaking changes**.

### Breaking changes

- **`Path[T].Key` is no longer an exported field.** Use `p.SetKey(name)`
  to assign and `p.Key()` to read. Mint itself never relied on user code
  setting `Key` directly — the field would simply be overwritten — so
  this aligns the API with reality.
- **Request bodies are now capped at 5 MiB by default.** JSON and form
  bodies above the limit return `413 body_too_large`. Override with
  `m.WithMaxRequestBodySize(n)`. Pass `0` to disable (not recommended).
- **Default JSON encoder no longer HTML-escapes `<`, `>`, `&`.** Output
  uses `json.Encoder` with `SetEscapeHTML(false)`. This matches what
  most APIs expect, but if you embed responses in HTML pages you need to
  escape them yourself. Restore the old behavior with
  `m.WithJSONMarshal(json.Marshal)`.
- **Plain 4xx errors now preserve their message in the response.**
  Previously, only `*HTTPError` could populate `Message`. A generic
  `errors.New("user not found")` now produces
  `{"err":"not_found","message":"user not found"}`. 5xx errors continue
  to hide their message (they are logged server-side).
- **`H()` panics use plain `panic()` instead of `log.Panic`.** Stack
  traces now go to stderr the way Go programmers expect — no double
  reporting via the logger.

### New features

- **`Header[T]` extractor.** Map HTTP request headers into a struct via
  `header:"X-Name"` tags. Validation tags are honored.
- **`Cookie[T]` extractor.** Same, but for cookies (`cookie:"name"`).
- **`ResponseWriter` explicitly implements `http.Flusher`,
  `http.Hijacker`, and `http.Pusher`.** SSE, WebSocket upgrades, and
  HTTP/2 push now work without `http.NewResponseController`. Calls
  return `http.ErrNotSupported` when the underlying writer does not
  support the operation.
- **Pattern parsing is cached.** `extractPatternNames` no longer
  re-parses `r.Pattern` on every request.
- **Better error reporting for path/handler mismatches.** If a handler
  declares more `Path[T]` parameters than the route pattern provides,
  Mint returns 400 instead of panicking, and logs a one-time warning.

### Fixes / hardening

- `Initialize` now logs a warning when called more than once (previously
  silent no-op).
- `Initialize` auto-creates the validator when `EnableValidation` is
  true and `Validator` is nil — the same logic `Configure` already had.
- Request bodies are explicitly closed in `JSON.Extract` to avoid
  relying on `net/http`'s deferred cleanup.
- Validator tag-name lookup now also considers `header` and `cookie`
  tags.
- `WithJSONMarshal` and `WithJSONEncode` no longer race: setting one
  unsets the other, so the option you set last is the one that takes
  effect.

### Migration

| Before                          | After                                  |
|---------------------------------|----------------------------------------|
| `p := m.Path[int]{Key: "id"}`   | not supported; Mint sets the key itself |
| `p.Key`                         | `p.Key()`                              |
| `errors.New("...")` → 500 body included `message` | now hidden for 5xx, kept for 4xx |
| 1 GB POST body → OOM            | 413 by default; configurable           |
| `json.Marshal` HTML-escaped output | unescaped by default; opt back in with `m.WithJSONMarshal(json.Marshal)` |

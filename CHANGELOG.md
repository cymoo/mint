# Changelog

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

# Mint

> A small, type-safe Go web toolkit built on top of `net/http`.

Mint is **not a framework**. It does not give you a router, a middleware chain,
or a request context type. What it gives you is one thing: a way to write HTTP
handlers as plain, type-safe Go functions.

```go
func GetUser(id m.Path[int]) (User, error) { ... }
func CreateUser(body m.JSON[NewUser]) m.Result[User] { ... }
func Search(q m.Query[Filter]) []Result { ... }
```

You keep `http.ServeMux`. You keep `http.HandlerFunc`. Mint is a thin adapter
in between that does parameter extraction, validation, and response
serialization for you — and gets out of the way for everything else.

```bash
go get github.com/cymoo/mint
```

Requires **Go 1.23+** (for the enhanced routing patterns in `net/http`).

---

## Table of Contents

- [Hello, Mint](#hello-mint)
- [Core idea: `H(handler)`](#core-idea-hhandler)
- [Extracting input](#extracting-input)
  - [`JSON[T]`](#jsont)
  - [`Query[T]`](#queryt)
  - [`Form[T]`](#formt)
  - [`File`](#file)
  - [`Path[T]`](#patht)
  - [`Header[T]`](#headert)
  - [`Cookie[T]`](#cookiet)
  - [Raw access](#raw-access)
  - [Custom extractors](#custom-extractors)
- [Returning output](#returning-output)
- [Errors](#errors)
- [Validation](#validation)
- [Streaming, SSE, hijack](#streaming-sse-hijack)
- [Configuration](#configuration)
- [Pitfalls & limitations](#pitfalls--limitations)
- [What Mint does not do](#what-mint-does-not-do)

---

## Hello, Mint

```go
package main

import (
    "net/http"

    m "github.com/cymoo/mint"
)

func hello(name m.Query[struct {
    Name string `schema:"name"`
}]) string {
    if name.Value.Name == "" {
        return "Hello, world!"
    }
    return "Hello, " + name.Value.Name + "!"
}

func main() {
    mux := http.NewServeMux()
    mux.HandleFunc("GET /hello", m.H(hello))
    http.ListenAndServe(":8080", mux)
}
```

That's it. No `Engine`, no `Router`, no `app := mint.New()`.

---

## Core idea: `H(handler)`

`m.H` adapts any function whose **parameters and return values** Mint
understands into an `http.HandlerFunc`:

```
http.HandleFunc(pattern, m.H(yourFunc))
```

### Allowed parameters

| Parameter type            | What it gives you                              |
|---------------------------|------------------------------------------------|
| `m.JSON[T]`               | Decoded + validated JSON body                  |
| `m.Query[T]`              | Decoded + validated query string               |
| `m.Form[T]`               | Decoded + validated form (`application/x-www-form-urlencoded` or `multipart/form-data`) |
| `m.File`                  | First uploaded file from a multipart form       |
| `m.Path[T]`               | A single typed path parameter                  |
| `m.Header[T]`             | Headers mapped onto a struct via `header` tags |
| `m.Cookie[T]`             | Cookies mapped onto a struct via `cookie` tags |
| `http.ResponseWriter`     | The raw writer (wrapped, see below)            |
| `*http.Request`           | The raw request                                |
| any type implementing `Extractor` | Your own extractor                      |

Mint **panics at registration time** if you use anything else as a
parameter. Bad handlers blow up at startup, not at request time.

### Allowed return shapes

| Return                       | Behavior                                             |
|------------------------------|------------------------------------------------------|
| _none_                       | 200, empty body                                       |
| `T`                          | JSON-encoded (or special-cased below)                |
| `error`                      | Error pipeline (see [Errors](#errors))               |
| `(T, error)`                 | Common case: data on success, error on failure       |
| `m.Result[T]`                | Full control: code, headers, body, or error         |
| `m.StatusCode`               | Empty body with this status                          |
| `m.HTML`                     | `Content-Type: text/html`                            |
| `[]byte`                     | `Content-Type: application/octet-stream`             |
| `io.Reader`                  | Streamed verbatim                                    |
| `http.Handler`               | Delegated to                                         |
| any type implementing `Responder` | Calls `Respond(w, r)`                           |

Any other concrete type is JSON-encoded. The first value of `(T, error)`
**cannot** be `Result[U]` — pick one style or the other.

---

## Extracting input

All extractors are tiny generic wrappers. Their decoded value lives in
`.Value`.

### `JSON[T]`

```go
type CreateUser struct {
    Name  string `json:"name"  validate:"required,min=2"`
    Email string `json:"email" validate:"required,email"`
}

func createUser(body m.JSON[CreateUser]) (User, error) {
    return saveUser(body.Value), nil
}
```

- Body is automatically size-limited (default 5 MiB; configurable).
- Empty body → `empty_body` error.
- Malformed JSON → 400 with `Err: "json_decode_error"`.
- Validation tags are honored.

### `Query[T]`

```go
type Filter struct {
    Page  int    `schema:"page"  validate:"gte=1"`
    Limit int    `schema:"limit" validate:"gte=1,lte=100"`
    Q     string `schema:"q"`
}

func search(f m.Query[Filter]) []Hit { ... }
```

Query strings are decoded with `gorilla/schema` using `schema` tags.

### `Form[T]`

```go
type Login struct {
    Username string `schema:"username" validate:"required"`
    Password string `schema:"password" validate:"required,min=8"`
}

func login(f m.Form[Login]) error { ... }
```

Same conventions as `Query[T]`, but reads `r.Form` after parsing URL-encoded
or multipart form data.

Multipart file fields can be bound by `schema`/`form` tag to `m.FilePart`,
`*m.FilePart`, `[]m.FilePart`, or `[]*m.FilePart`:

```go
type Upload struct {
    Title string     `schema:"title"`
    File  m.FilePart `schema:"file"`
}

func upload(f m.Form[Upload]) error {
    name := filepath.Base(f.Value.File.Filename)
    return f.Value.File.Save(filepath.Join("./uploads", name))
}
```

`FilePart.Save(path)` streams the upload to disk, creates parent directories,
removes a partial target on failure, and can only be called once. Use
`FilePart.Open()` if you need to stream the file yourself.

### `File`

```go
func upload(file m.File) error {
    name := filepath.Base(file.Value.Filename)
    return file.Save(filepath.Join("./uploads", name))
}
```

`File` extracts the first uploaded file from a `multipart/form-data` request.
Use `Form[T]` with `FilePart` fields when you need to bind a specific form
field name or accept multiple files.

### `Path[T]`

`Path[T]` extracts a single typed path parameter:

```go
mux.HandleFunc("GET /users/{id}", m.H(func(id m.Path[int]) User { ... }))
mux.HandleFunc("GET /files/{name}", m.H(func(name m.Path[string]) File { ... }))
```

Supported types: `string`, `int`, `int8`, `int16`, `int32`, `int64`,
`uint`–`uint64`, `float32`, `float64`, `bool`.

> ⚠️ **Path binding is positional, not by name.** Path params are matched to
> handler parameters **in order**. For
> `mux.HandleFunc("GET /users/{uid}/posts/{pid}", m.H(handler))`, the **first**
> `Path[T]` parameter in `handler` receives `{uid}`, the **second** receives
> `{pid}` — regardless of the Go variable names you choose.
>
> ```go
> // pid gets {uid}, uid gets {pid}. Don't do this:
> func bad(pid m.Path[int], uid m.Path[int]) { ... }
>
> // Match argument order to URL order:
> func good(uid m.Path[int], pid m.Path[int]) { ... }
> ```

If you declare more `Path[T]` parameters than the route pattern provides,
Mint returns **400** at request time (and logs a one-time warning).

### `Header[T]`

```go
type AuthHeaders struct {
    Token     string `header:"Authorization" validate:"required"`
    RequestID string `header:"X-Request-ID"`
}

func protected(h m.Header[AuthHeaders]) string {
    return "hello, " + h.Value.Token
}
```

Missing headers are zero values. Use `validate:"required"` if you need
them present.

### `Cookie[T]`

```go
type Session struct {
    ID    string `cookie:"session_id" validate:"required"`
    Theme string `cookie:"theme"`
}

func me(s m.Cookie[Session]) User { ... }
```

### Raw access

You can mix extractors with `http.ResponseWriter` and `*http.Request`
freely:

```go
func download(id m.Path[int], w http.ResponseWriter, r *http.Request) error {
    f, err := openFile(id.Value)
    if err != nil { return err }
    defer f.Close()
    http.ServeContent(w, r, f.Name(), f.ModTime(), f)
    return nil
}
```

### Custom extractors

Any type that implements `Extract(*http.Request) error` (on a pointer
receiver) works:

```go
type Authed struct{ User User }

func (a *Authed) Extract(r *http.Request) error {
    u, err := userFromBearer(r.Header.Get("Authorization"))
    if err != nil { return &m.HTTPError{Code: 401, Err: "unauthorized"} }
    a.User = u
    return nil
}

func me(a Authed) User { return a.User }
```

---

## Returning output

The most common pattern is `(T, error)`:

```go
func getUser(id m.Path[int]) (User, error) {
    u, ok := users[id.Value]
    if !ok {
        return User{}, &m.HTTPError{Code: 404, Err: "not_found"}
    }
    return u, nil
}
```

When you need to control the status code or headers, use `Result[T]`:

```go
func createUser(body m.JSON[NewUser]) m.Result[User] {
    u := save(body.Value)
    return m.Result[User]{
        Code: 201,
        Headers: http.Header{
            "Location": {fmt.Sprintf("/users/%d", u.ID)},
        },
        Data: u,
    }
}
```

There are helpers `m.OK[T](data)` and `m.Err[T](code, err)` for trivial
cases.

### Special return types

```go
func deleted() m.StatusCode { return 204 }
func page() m.HTML          { return "<h1>Hi</h1>" }
func csv() []byte           { return []byte("a,b,c\n") }
func bigFile() io.Reader    { f, _ := os.Open("x"); return f }
```

---

## Errors

Mint converts errors into HTTP responses in this order of priority:

1. **`*HTTPError`** — used verbatim.

   ```go
   return &m.HTTPError{Code: 404, Err: "not_found", Message: "user 42 not found"}
   ```

2. **`*ExtractError`** — emitted by built-in/custom extractors. Mint maps
   the `Type` to a code (`body_too_large` → 413, etc.).

3. **Anything else** — Mint inspects the error message to guess a status:
   - "not found" / "doesn't exist" → 404
   - "unauthorized" / "auth"        → 401
   - "forbidden"                    → 403
   - "timeout"                      → 408
   - "conflict" / "exists"          → 409
   - "invalid" / "validation"       → 400
   - otherwise                      → 500

### Visibility rules

| Status range | `Message` in response body | Logged?           |
|--------------|----------------------------|-------------------|
| 4xx          | Yes — set to `err.Error()` | No                |
| 5xx          | **Hidden** (empty)         | Yes (`mint: 5xx`) |

This way internal errors don't leak to clients, but you still see them
in logs.

> If you want exact control: return `*HTTPError`. Mint will respect both
> `Err` and `Message` regardless of code.

---

## Validation

Validation is **on by default** and uses
[`go-playground/validator`](https://github.com/go-playground/validator).
The framework wires up tag-name lookup for `json`, `schema`, `header`,
and `cookie` tags so error messages reference the **input** name, not
the Go field name.

```go
type Body struct {
    Email string `json:"email" validate:"required,email"`
}

// On bad input:
// 400 {"err":"validation_failed","message":"email: required validation failed"}
```

Turn it off with `m.Configure(m.WithValidation(false))` or use a custom
validator with `m.WithValidator(...)`.

---

## Streaming, SSE, hijack

The `ResponseWriter` that handlers receive is a thin wrapper that still
forwards to the underlying `http.ResponseWriter`. It explicitly
implements:

- `http.Flusher` — for SSE / `text/event-stream`
- `http.Hijacker` — for protocol upgrades (WebSocket, etc.)
- `http.Pusher` — for HTTP/2 push

Each delegates to the underlying writer, or returns
`http.ErrNotSupported` if the underlying server doesn't support it.

```go
func sse(w http.ResponseWriter, r *http.Request) {
    w.Header().Set("Content-Type", "text/event-stream")
    w.Header().Set("Cache-Control", "no-cache")
    flusher, _ := w.(http.Flusher)
    for i := 0; i < 5; i++ {
        fmt.Fprintf(w, "data: tick %d\n\n", i)
        flusher.Flush()
        time.Sleep(time.Second)
    }
}
```

---

## Configuration

Configuration is **global** and thread-safe. Set it once at startup:

```go
m.Initialize(
    m.WithMaxRequestBodySize(10 << 20), // 10 MiB
    m.WithLogger(myLogger),
)
```

`Initialize` should be called at most once. Use `Configure` to override
later (e.g. in tests). `Reset` returns everything to defaults.

| Option                         | Default                                 |
|--------------------------------|------------------------------------------|
| `WithMaxRequestBodySize(n)`    | `5 << 20` (5 MiB). Pass `0` to disable. |
| `WithJSONEncode(fn)`           | `json.Encoder` with `SetEscapeHTML(false)` |
| `WithJSONMarshal(fn)`          | _(unset; falls back to encoder)_         |
| `WithJSONUnmarshal(fn)`        | `json.Unmarshal`                         |
| `WithSchemaDecoder(d)`         | `schema.Decoder` with `IgnoreUnknownKeys(true)` |
| `WithValidation(bool)`         | `true`                                   |
| `WithValidator(v)`             | auto-created when validation enabled     |
| `WithLogger(l)`                | `log.Default()`                          |
| `WithErrorHandler(fn)`         | built-in (returns JSON `HTTPError`)      |

---

## Pitfalls & limitations

A short, honest list of things that will bite you if you don't know
them.

**1. `Path[T]` binds positionally, not by name.**
See the [`Path[T]`](#patht) section. The order of `Path[T]` parameters
in your function must match the left-to-right order of `{name}` in the
URL pattern. The Go variable names are ignored.

**2. Request body is capped at 5 MiB by default.**
JSON and form bodies above the limit get 413
`{"err":"body_too_large"}`. Configure with
`m.WithMaxRequestBodySize(n)`. Set to `0` to disable (not recommended).

**3. 5xx error messages are hidden from clients by default.**
Generic errors with a 5xx-mapped status produce a body without
`Message`. The full error is logged. Return an `*HTTPError` if you
intentionally want the message in the response.

**4. JSON output does not escape `<`, `>`, `&` by default.**
Mint's default encoder calls `json.Encoder.SetEscapeHTML(false)`. If you
embed user-controlled JSON inside an HTML page, escape it yourself.
Restore the standard behavior with `m.WithJSONMarshal(json.Marshal)`.

**5. Configuration is global.**
Calling `Configure` from multiple goroutines is safe, but it affects
every request. Don't tweak it per-request.

**6. `Initialize` should be called once.**
A second call is ignored — Mint will log a warning telling you so. Use
`Configure` to layer further changes, or `Reset` (in tests).

**7. `ResponseWriter` is wrapped.**
Type-asserting to your own custom interface won't work. Use
`http.NewResponseController(w)` or the explicit methods Mint exposes
(`Flush`, `Hijack`, `Push`). Call `Unwrap()` if you need the raw writer.

**8. Handler signatures are checked at registration, not compile time.**
A bad signature passed to `m.H` panics when `H` runs. Register all
routes at startup so problems surface immediately.

---

## What Mint does not do

By design:

- **No router.** Use `http.ServeMux` (Go 1.22+ patterns) or anything
  else that produces `http.HandlerFunc`.
- **No middleware system.** Use `func(http.Handler) http.Handler`
  middleware — Mint's adapter is itself just an `http.HandlerFunc`.
- **No request context wrapper.** Use `r.Context()`.
- **No DI container, no app object, no global state beyond config.**

If you want any of these, pick a heavier framework. If you want plain
Go with less boilerplate, Mint is for you.

---

## License

MIT — see `LICENSE`.

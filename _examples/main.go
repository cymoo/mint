// Package main is a runnable tour of every Mint feature.
//
// Run it with:
//
//	go run ./_examples
//
// Then poke at it with curl. Each handler below has a comment showing
// the example request and expected response.
package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	m "github.com/cymoo/mint"
)

// ============================================================================
// Domain types
// ============================================================================

type User struct {
	ID    int    `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

type CreateUserBody struct {
	Name  string `json:"name"  validate:"required,min=2,max=50"`
	Email string `json:"email" validate:"required,email"`
}

type UpdateUserBody struct {
	Name  string `json:"name,omitempty"  validate:"omitempty,min=2,max=50"`
	Email string `json:"email,omitempty" validate:"omitempty,email"`
}

type ListUsersQuery struct {
	Page  int    `schema:"page"  validate:"gte=0"`
	Limit int    `schema:"limit" validate:"gte=0,lte=100"`
	Sort  string `schema:"sort"  validate:"omitempty,oneof=name email id"`
}

type LoginForm struct {
	Username string `schema:"username" validate:"required"`
	Password string `schema:"password" validate:"required,min=8"`
}

type AuthHeaders struct {
	Token     string `header:"Authorization" validate:"required"`
	RequestID string `header:"X-Request-ID"`
}

type SessionCookies struct {
	SessionID string `cookie:"session_id" validate:"required"`
	Theme     string `cookie:"theme"`
}

// ============================================================================
// In-memory store
// ============================================================================

var (
	storeMu sync.RWMutex
	users   = map[int]User{
		1: {ID: 1, Name: "Alice", Email: "alice@example.com"},
		2: {ID: 2, Name: "Bob", Email: "bob@example.com"},
	}
	nextID atomic.Int32
)

func init() { nextID.Store(3) }

// ============================================================================
// Wiring
// ============================================================================

func main() {
	m.Initialize(
		m.WithMaxRequestBodySize(1 << 20), // 1 MiB — tighter than default
	)

	mux := http.NewServeMux()

	// Plain responses
	mux.HandleFunc("GET /", m.H(home))
	mux.HandleFunc("GET /html", m.H(htmlPage))

	// CRUD with JSON bodies and validation
	mux.HandleFunc("GET    /users", m.H(listUsers))
	mux.HandleFunc("GET    /users/{id}", m.H(getUser))
	mux.HandleFunc("POST   /users", m.H(createUser))
	mux.HandleFunc("PUT    /users/{id}", m.H(updateUser))
	mux.HandleFunc("DELETE /users/{id}", m.H(deleteUser))

	// Query, Form, multi-Path
	mux.HandleFunc("GET  /search", m.H(search))
	mux.HandleFunc("POST /login", m.H(login))
	mux.HandleFunc("GET  /calc/{a}/{b}", m.H(calc))

	// Header & Cookie extractors
	mux.HandleFunc("GET /me", m.H(me))            // Authorization header
	mux.HandleFunc("GET /session", m.H(session))  // session_id cookie

	// Custom Result, status-only, binary, HTML
	mux.HandleFunc("GET /custom", m.H(customResult))
	mux.HandleFunc("GET /status", m.H(statusOnly))
	mux.HandleFunc("GET /csv", m.H(csv))
	mux.HandleFunc("GET /download/{name}", m.H(download))

	// Error patterns
	mux.HandleFunc("GET /err/generic", m.H(genericErr))
	mux.HandleFunc("GET /err/4xx", m.H(http4xx))
	mux.HandleFunc("GET /err/5xx", m.H(http5xx))
	mux.HandleFunc("GET /err/teapot", m.H(teapot))

	// SSE — uses http.Flusher through the wrapped ResponseWriter
	mux.HandleFunc("GET /sse", m.H(sse))

	// Raw http.ResponseWriter + *http.Request
	mux.HandleFunc("GET /raw", m.H(raw))

	log.Println("🌿 Mint demo on http://localhost:8080")
	log.Fatal(http.ListenAndServe(":8080", mux))
}

// ============================================================================
// Handlers
// ============================================================================

// home: simplest possible handler — return a string.
//
//   curl http://localhost:8080/
//   "Welcome to Mint!"
func home() string {
	return "Welcome to Mint!"
}

// htmlPage: m.HTML sets Content-Type: text/html.
//
//   curl http://localhost:8080/html
func htmlPage() m.HTML {
	return `<h1>Hello</h1><p>This is an HTML response.</p>`
}

// listUsers: query parameters with validation.
//
//   curl 'http://localhost:8080/users?page=1&limit=20&sort=name'
func listUsers(q m.Query[ListUsersQuery]) []User {
	storeMu.RLock()
	defer storeMu.RUnlock()
	out := make([]User, 0, len(users))
	for _, u := range users {
		out = append(out, u)
	}
	return out
}

// getUser: a single typed path parameter + (T, error) return.
//
//   curl http://localhost:8080/users/1
//   curl http://localhost:8080/users/999  # 404
func getUser(id m.Path[int]) (User, error) {
	storeMu.RLock()
	defer storeMu.RUnlock()
	u, ok := users[id.Value]
	if !ok {
		return User{}, &m.HTTPError{
			Code:    404,
			Err:     "not_found",
			Message: fmt.Sprintf("user %d not found", id.Value),
		}
	}
	return u, nil
}

// createUser: JSON body extraction + validation + Result with custom status/headers.
//
//   curl -X POST http://localhost:8080/users \
//        -H 'Content-Type: application/json' \
//        -d '{"name":"Eve","email":"eve@example.com"}'
func createUser(body m.JSON[CreateUserBody]) m.Result[User] {
	storeMu.Lock()
	defer storeMu.Unlock()
	u := User{ID: int(nextID.Add(1)) - 1, Name: body.Value.Name, Email: body.Value.Email}
	users[u.ID] = u
	return m.Result[User]{
		Code: 201,
		Headers: http.Header{
			"Location": {fmt.Sprintf("/users/%d", u.ID)},
		},
		Data: u,
	}
}

// updateUser: Path[T] + JSON[T] together.
//
//   curl -X PUT http://localhost:8080/users/1 \
//        -H 'Content-Type: application/json' \
//        -d '{"name":"Alice Jr."}'
func updateUser(id m.Path[int], body m.JSON[UpdateUserBody]) (User, error) {
	storeMu.Lock()
	defer storeMu.Unlock()
	u, ok := users[id.Value]
	if !ok {
		return User{}, &m.HTTPError{Code: 404, Err: "not_found"}
	}
	if body.Value.Name != "" {
		u.Name = body.Value.Name
	}
	if body.Value.Email != "" {
		u.Email = body.Value.Email
	}
	users[id.Value] = u
	return u, nil
}

// deleteUser: status-only return.
//
//   curl -X DELETE http://localhost:8080/users/1 -i
func deleteUser(id m.Path[int]) (m.StatusCode, error) {
	storeMu.Lock()
	defer storeMu.Unlock()
	if _, ok := users[id.Value]; !ok {
		return 0, &m.HTTPError{Code: 404, Err: "not_found"}
	}
	delete(users, id.Value)
	return 204, nil
}

// search: read-only query handler.
//
//   curl 'http://localhost:8080/search?q=alice&page=1&limit=10'
func search(q m.Query[struct {
	Q     string `schema:"q"`
	Page  int    `schema:"page"  validate:"gte=0"`
	Limit int    `schema:"limit" validate:"gte=0,lte=100"`
}]) map[string]any {
	return map[string]any{
		"q":     q.Value.Q,
		"page":  q.Value.Page,
		"limit": q.Value.Limit,
	}
}

// login: form data extraction.
//
//   curl -X POST http://localhost:8080/login \
//        -d 'username=alice&password=hunter22'
func login(f m.Form[LoginForm]) map[string]string {
	return map[string]string{
		"status":   "ok",
		"username": f.Value.Username,
		"token":    "demo-token",
	}
}

// calc: multiple path parameters with different types.
//
// ⚠️ Positional binding: the first Path[T] gets {a}, the second gets {b}.
//
//   curl http://localhost:8080/calc/3/4.5
func calc(a m.Path[int], b m.Path[float64]) map[string]any {
	return map[string]any{
		"sum":     float64(a.Value) + b.Value,
		"product": float64(a.Value) * b.Value,
	}
}

// me: Header[T] extraction for an Authorization header.
//
//   curl http://localhost:8080/me -H 'Authorization: Bearer xyz' -H 'X-Request-ID: r1'
func me(h m.Header[AuthHeaders]) map[string]string {
	return map[string]string{
		"token":      h.Value.Token,
		"request_id": h.Value.RequestID,
	}
}

// session: Cookie[T] extraction.
//
//   curl http://localhost:8080/session --cookie 'session_id=abc; theme=dark'
func session(c m.Cookie[SessionCookies]) map[string]string {
	return map[string]string{
		"session_id": c.Value.SessionID,
		"theme":      c.Value.Theme,
	}
}

// customResult: Result with custom headers.
//
//   curl -i http://localhost:8080/custom
func customResult() m.Result[map[string]any] {
	return m.Result[map[string]any]{
		Code: 200,
		Headers: http.Header{
			"X-Custom": {"hi"},
			"X-Time":   {time.Now().UTC().Format(time.RFC3339)},
		},
		Data: map[string]any{"ok": true},
	}
}

// statusOnly: bare m.StatusCode.
//
//   curl -i http://localhost:8080/status
func statusOnly() m.StatusCode { return 202 }

// csv: []byte body. Use Content-Type via raw access for non-octet types.
//
//   curl http://localhost:8080/csv
func csv() []byte {
	var b strings.Builder
	b.WriteString("id,name,email\n")
	storeMu.RLock()
	defer storeMu.RUnlock()
	for _, u := range users {
		fmt.Fprintf(&b, "%d,%s,%s\n", u.ID, u.Name, u.Email)
	}
	return []byte(b.String())
}

// download: stream from an io.Reader.
//
//   curl http://localhost:8080/download/hello.txt
func download(name m.Path[string]) io.Reader {
	return strings.NewReader("contents of " + name.Value + "\n")
}

// genericErr: a plain error → Mint guesses status from the message.
//
//   curl -i http://localhost:8080/err/generic
//   # 500, message hidden, original logged
func genericErr() error {
	return errors.New("something internal failed")
}

// http4xx: 4xx errors keep their message in the response.
//
//   curl -i http://localhost:8080/err/4xx
func http4xx() error {
	return errors.New("invalid request: missing field x")
}

// http5xx: explicit *HTTPError lets you override the default visibility.
//
//   curl -i http://localhost:8080/err/5xx
func http5xx() error {
	return &m.HTTPError{Code: 503, Err: "unavailable", Message: "DB is down"}
}

// teapot: any custom code via *HTTPError.
//
//   curl -i http://localhost:8080/err/teapot
func teapot() error {
	return &m.HTTPError{Code: 418, Err: "teapot", Message: "I'm a teapot"}
}

// sse: Server-Sent Events using the wrapped ResponseWriter's Flusher.
//
//   curl -N http://localhost:8080/sse
func sse(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", 500)
		return
	}

	for i := 0; i < 5; i++ {
		select {
		case <-r.Context().Done():
			return
		default:
		}
		fmt.Fprintf(w, "data: tick %d\n\n", i)
		flusher.Flush()
		time.Sleep(500 * time.Millisecond)
	}
}

// raw: full access to ResponseWriter and Request — no Mint magic.
//
//   curl -i http://localhost:8080/raw
func raw(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Raw", "yes")
	fmt.Fprintln(w, "Method:", r.Method)
	fmt.Fprintln(w, "URL:   ", r.URL)
}

package main

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github/cymoo/mint"
)

// Domain models
type User struct {
	ID    int    `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

type CreateUserRequest struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

type UpdateUserRequest struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email,omitempty"`
}

type UserQueryParams struct {
	Page  int    `schema:"page"`
	Limit int    `schema:"limit"`
	Sort  string `schema:"sort"`
}

type LoginForm struct {
	Username string `schema:"username"`
	Password string `schema:"password"`
}

// Simple in-memory storage
var (
	users = map[int]User{
		1: {ID: 1, Name: "Alice", Email: "alice@example.com"},
		2: {ID: 2, Name: "Bob", Email: "bob@example.com"},
	}
	nextID = 3
)

func main() {
	mux := http.NewServeMux()

	// Basic responses
	mux.HandleFunc("GET /", m.H(handleHome))
	mux.HandleFunc("GET /html", m.H(handleHTML))

	// RESTful API with automatic parameter extraction
	mux.HandleFunc("GET /api/users", m.H(handleListUsers))
	mux.HandleFunc("GET /api/users/{id}", m.H(handleGetUser))
	mux.HandleFunc("POST /api/users", m.H(handleCreateUser))
	mux.HandleFunc("PUT /api/users/{id}", m.H(handleUpdateUser))
	mux.HandleFunc("DELETE /api/users/{id}", m.H(handleDeleteUser))

	// Different parameter extraction types
	mux.HandleFunc("GET /api/search", m.H(handleSearch))     // Query params
	mux.HandleFunc("POST /api/login", m.H(handleLogin))      // Form data
	mux.HandleFunc("GET /api/calc/{a}/{b}", m.H(handleCalc)) // Multiple path params

	// Advanced response handling
	mux.HandleFunc("GET /api/custom", m.H(handleCustomResult)) // Result with headers
	mux.HandleFunc("GET /api/status", m.H(handleStatusOnly))   // StatusCode only
	mux.HandleFunc("GET /api/binary", m.H(handleBinary))       // Binary response

	// Error handling patterns
	mux.HandleFunc("GET /api/error", m.H(handleError))            // Generic error
	mux.HandleFunc("GET /api/http-error", m.H(handleHTTPError))   // Custom HTTP error
	mux.HandleFunc("GET /api/users/{id}/check", m.H(handleCheck)) // (data, error) pattern

	// Direct access to http primitives
	mux.HandleFunc("GET /api/raw", m.H(handleRaw))

	log.Println("🚀 Server running at http://localhost:8080")
	log.Fatal(http.ListenAndServe(":8080", mux))
}

// ============================================================================
// Handler Functions - Demonstrating framework features
// ============================================================================

// 1. Simple string response
func handleHome() string {
	return "Welcome to the API! 🎉"
}

// 2. HTML response
func handleHTML() m.HTML {
	return m.HTML("<h1>Hello World</h1><p>This is HTML content</p>")
}

// 3. Return struct (auto JSON serialization)
func handleListUsers(q m.Query[UserQueryParams]) []User {
	log.Printf("Query: page=%d, limit=%d, sort=%s", q.Value.Page, q.Value.Limit, q.Value.Sort)

	result := make([]User, 0, len(users))
	for _, u := range users {
		result = append(result, u)
	}
	return result
}

// 4. Path parameter extraction + error handling
func handleGetUser(id m.Path[int]) (User, error) {
	user, ok := users[id.Value]
	if !ok {
		return User{}, &m.HTTPError{
			Code:    404,
			Err:     "not_found",
			Message: fmt.Sprintf("user %d not found", id.Value),
		}
	}
	return user, nil
}

// 5. JSON body extraction + Result with custom status and headers
func handleCreateUser(body m.JSON[CreateUserRequest]) m.Result[User] {
	user := User{
		ID:    nextID,
		Name:  body.Value.Name,
		Email: body.Value.Email,
	}
	users[nextID] = user
	nextID++

	return m.Result[User]{
		Code: 201, // Custom status code
		Headers: http.Header{
			"Location": []string{fmt.Sprintf("/api/users/%d", user.ID)},
		},
		Data: user,
	}
}

// 6. Path param + JSON body (two-value return pattern)
func handleUpdateUser(id m.Path[int], body m.JSON[UpdateUserRequest]) (User, error) {
	user, ok := users[id.Value]
	if !ok {
		return User{}, &m.HTTPError{Code: 404, Err: "not_found"}
	}

	if body.Value.Name != "" {
		user.Name = body.Value.Name
	}
	if body.Value.Email != "" {
		user.Email = body.Value.Email
	}
	users[id.Value] = user

	return user, nil
}

// 7. Return StatusCode only (useful for 204 No Content)
func handleDeleteUser(id m.Path[int]) (m.StatusCode, error) {
	if _, ok := users[id.Value]; !ok {
		return 0, &m.HTTPError{Code: 404, Err: "not_found"}
	}
	delete(users, id.Value)
	return m.StatusCode(204), nil
}

// 8. Query parameter extraction
func handleSearch(q m.Query[UserQueryParams]) map[string]any {
	return map[string]any{
		"page":  q.Value.Page,
		"limit": q.Value.Limit,
		"sort":  q.Value.Sort,
		"info":  "Query parameters extracted automatically",
	}
}

// 9. Form data extraction
func handleLogin(form m.Form[LoginForm]) map[string]string {
	return map[string]string{
		"status":   "success",
		"username": form.Value.Username,
		"token":    "demo-token-12345",
	}
}

// 10. Multiple path parameters with different types
func handleCalc(a m.Path[int], b m.Path[float64]) map[string]any {
	return map[string]any{
		"a":       a.Value,
		"b":       b.Value,
		"sum":     float64(a.Value) + b.Value,
		"product": float64(a.Value) * b.Value,
	}
}

// 11. Result type with custom headers
func handleCustomResult() m.Result[map[string]any] {
	return m.Result[map[string]any]{
		Code: 200,
		Headers: http.Header{
			"X-Custom-Header": []string{"custom-value"},
			"X-Timestamp":     []string{time.Now().Format(time.RFC3339)},
		},
		Data: map[string]any{
			"message": "Response with custom headers",
			"time":    time.Now(),
		},
	}
}

// 12. Return only status code
func handleStatusOnly() m.StatusCode {
	return m.StatusCode(202) // 202 Accepted
}

// 13. Binary response ([]byte)
func handleBinary() []byte {
	csv := "ID,Name,Email\n"
	for _, u := range users {
		csv += fmt.Sprintf("%d,%s,%s\n", u.ID, u.Name, u.Email)
	}
	return []byte(csv)
}

// 14. Generic error (framework infers status code from message)
func handleError() error {
	return errors.New("something went wrong")
}

// 15. Custom HTTP error with specific code
func handleHTTPError() error {
	return &m.HTTPError{
		Code:    418,
		Err:     "teapot",
		Message: "I'm a teapot",
	}
}

// 16. Two-value return: (data, error) pattern
func handleCheck(id m.Path[int]) (map[string]any, error) {
	user, ok := users[id.Value]
	if !ok {
		return nil, &m.HTTPError{Code: 404, Err: "not_found"}
	}

	return map[string]any{
		"user_id":    user.ID,
		"valid":      true,
		"checked_at": time.Now(),
	}, nil
}

// 17. Direct access to http.ResponseWriter and *http.Request
func handleRaw(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Custom", "direct-access")
	fmt.Fprintf(w, "Direct access to ResponseWriter and Request\n")
	fmt.Fprintf(w, "Method: %s\n", r.Method)
	fmt.Fprintf(w, "URL: %s\n", r.URL)
}

# Mint

A lightweight, type-safe Go web framework built on top of `net/http` with automatic parameter extraction and elegant response handling.

## ✨ Features

- 🚀 **Zero Learning Curve** - Built on standard `net/http`, no custom router
- 🎯 **Automatic Parameter Extraction** - JSON body, query params, form data, and path parameters
- ✅ **Automatic Validation** - Built-in validation with user-friendly error messages
- 🔒 **Type-Safe** - Leverages Go generics for compile-time type safety
- 📦 **Flexible Response Handling** - Return any type: structs, strings, HTML, status codes, or custom results
- ⚡ **Minimal Boilerplate** - Write handlers as simple functions
- 🛠️ **Customizable** - Configure JSON encoding, schema decoding, and error handling
- 🪶 **Lightweight** - No dependencies beyond gorilla/schema for form parsing

## 📦 Installation

```bash
go get github.com/cymoo/mint
```

**Requirements:** Go 1.23+ (for enhanced routing patterns)

## 🚀 Quick Start

```go
package main

import (
	"net/http"

	"github.com/cymoo/mint"
)

type User struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

type UpdateUserRequest struct {
	Name string `json:"name" validate:"required,min=2,max=50"`
}

// Simple string response
func handleHome() string {
	return "Hello, World!"
}

// JSON response with path parameter
func handleGetUser(id m.Path[int]) (User, error) {
	// id.Value contains the parsed integer
	return User{ID: id.Value, Name: "Alice"}, nil
}

// Result[T] for full control over the response with multiple parameters with different types
func handleUpdateUser(id m.Path[int], req m.JSON[UpdateUserRequest]) m.Result[*User] {
	return m.Result[*User]{
		Data: &User{ID: id.Value, Name: req.Value.Name},
		Code: http.StatusOK,
		Headers: http.Header{
			"X-Custom-Header": []string{"foo"},
		},
	}
}

func main() {
	mux := http.NewServeMux()

	mux.HandleFunc("/", m.H(handleHome))
	mux.HandleFunc("GET /users/{id}", m.H(handleGetUser))
	mux.HandleFunc("PUT /users/{id}", m.H(handleUpdateUser))

	err := http.ListenAndServe(":8080", mux)
	if err != nil {
		panic(err)
	}
}
```

See [_examples](./_examples/) for more detailed examples.

## 📚 Core Concepts

### The `H` Function

`m.H()` wraps your handler functions, enabling automatic parameter extraction and response handling:

```go
mux.HandleFunc("POST /users", m.H(handleCreateUser))
```

### Parameter Extractors

Extract data from requests using type-safe extractors:

| Extractor | Purpose | Example |
|-----------|---------|---------|
| `m.Path[T]` | Path parameters | `{id}` → `m.Path[int]` |
| `m.JSON[T]` | JSON request body | `m.JSON[CreateUserRequest]` |
| `m.Query[T]` | Query parameters | `?page=1` → `m.Query[Pagination]` |
| `m.Form[T]` | Form data | `username=...` → `m.Form[LoginForm]` |

### Response Types

Return values are automatically handled:

| Return Type | Result |
|-------------|--------|
| `string` | `text/plain` response |
| `m.HTML` | `text/html` response |
| `struct` / `map` / `slice` | `application/json` response |
| `m.StatusCode` | HTTP status code only |
| `[]byte` | `application/octet-stream` response |
| `m.Result[T]` | Custom status code + headers + data |
| `error` | Automatic error handling |
| `(T, error)` | Data or error pattern |

## 📖 Usage Examples

### Path Parameters

Extract typed path parameters from URLs:

```go
// Single parameter
mux.HandleFunc("GET /users/{id}", m.H(func(id m.Path[int]) (User, error) {
    user, ok := getUser(id.Value)
    if !ok {
        return User{}, &m.HTTPError{
            Code:    404,
            Err:     "not_found",
            Message: "user not found",
        }
    }
    return user, nil
}))

// Multiple parameters with different types
mux.HandleFunc("GET /calc/{a}/{b}", m.H(func(a m.Path[int], b m.Path[float64]) map[string]any {
    return map[string]any{
        "sum": float64(a.Value) + b.Value,
    }
}))
```

**Supported types:** `string`, `int`, `int64`, `uint`, `uint64`, `float64`, `bool`

### JSON Request Body

Parse JSON request bodies automatically:

```go
type CreateUserRequest struct {
    Name  string `json:"name"`
    Email string `json:"email"`
}

mux.HandleFunc("POST /users", m.H(func(body m.JSON[CreateUserRequest]) m.Result[User] {
    user := User{
        Name:  body.Value.Name,
        Email: body.Value.Email,
    }
    
    return m.Result[User]{
        Code: 201,
        Headers: http.Header{
            "Location": []string{"/users/" + user.ID},
        },
        Data: user,
    }
}))
```

### Query Parameters

Extract and parse query parameters:

```go
type Pagination struct {
    Page  int    `schema:"page"`
    Limit int    `schema:"limit"`
    Sort  string `schema:"sort"`
}

mux.HandleFunc("GET /users", m.H(func(q m.Query[Pagination]) []User {
    // Access via q.Value.Page, q.Value.Limit, etc.
    return getUsers(q.Value.Page, q.Value.Limit)
}))
```

### Form Data

Parse form submissions:

```go
type LoginForm struct {
    Username string `schema:"username"`
    Password string `schema:"password"`
}

mux.HandleFunc("POST /login", m.H(func(form m.Form[LoginForm]) map[string]string {
    // Authenticate user
    token := authenticate(form.Value.Username, form.Value.Password)
    return map[string]string{"token": token}
}))
```

### Custom Response with Headers

Use `m.Result[T]` for full control over the response:

```go
mux.HandleFunc("GET /download", m.H(func() m.Result[Data] {
    return m.Result[Data]{
        Code: 200,
        Headers: http.Header{
            "Content-Disposition": []string{"attachment; filename=data.json"},
            "X-Custom-Header":     []string{"value"},
        },
        Data: myData,
    }
}))
```

### Error Handling

Multiple ways to handle errors:

```go
// 1. Return generic error (status inferred from message)
func handler() error {
    return errors.New("not found") // → 404
}

// 2. Return custom HTTP error
func handler() error {
    return &m.HTTPError{
        Code:    400,
        Err:     "validation_error",
        Message: "invalid input",
    }
}

// 3. Two-value return pattern
func handler(id m.Path[int]) (User, error) {
    user, err := getUser(id.Value)
    if err != nil {
        return User{}, err
    }
    return user, nil
}

// 4. Result with error
func handler() m.Result[User] {
    return m.Err[User](400, errors.New("bad request"))
}
```

### Direct HTTP Access

When you need full control, access raw HTTP primitives:

```go
mux.HandleFunc("GET /custom", m.H(func(w http.ResponseWriter, r *http.Request) {
    w.Header().Set("X-Custom", "header")
    w.WriteHeader(200)
    w.Write([]byte("custom response"))
}))
```

## Custom Extractors Guide

Custom extractors allow you to extend the framework to handle any type of request data. Here's how to create your own:

### Basic Structure

Implement the `Extractor` interface with an `Extract` method:

```go
type BearerToken struct {
    Token string
}

func (bt *BearerToken) Extract(r *http.Request) error {
    const bearerPrefix = "Bearer "
    auth := r.Header.Get("Authorization")
    
    if strings.HasPrefix(auth, bearerPrefix) {
        bt.Token = strings.TrimSpace(auth[len(bearerPrefix):])
    }
    
    if bt.Token == "" {
        return &m.ExtractError{
            Type:    "invalid_authorization",
            Message: "Authorization header must be: Bearer <token>",
        }
    }
    
    return nil
}
```

### Key Features

- **Automatic Injection**: The framework automatically calls `Extract()` and injects the parsed value
- **Error Handling**: Return `ExtractError` with clear type and message for client errors
- **Type Safety**: Leverage Go's type system for validated, type-safe parameters

### Usage in Handlers

Simply include your custom extractor as a handler parameter:

```go
func mySecureApi(bearer BearerToken) string {
    return "token: " + bearer.Token
}
```

### Best Practices

- Keep extractors focused on single responsibility
- Return meaningful error messages for client-side issues
- Use `ExtractError` for consistent error handling
- Validate and sanitize data within the extractor

Custom extractors make your handlers cleaner by moving data extraction and validation logic to reusable components.

## ⚙️ Configuration

Mint provides flexible configuration options using the functional options pattern for clean, type-safe customization.

### Basic Setup

```go
import (
    "log"
    "github.com/cymoo/mint"
)

func main() {
    // Initialize configuration at application startup (recommended)
    m.Initialize(
        m.WithLogger(log.Default()),
        m.WithValidation(true),
    )
    
    // Your application code...
}
```

### Available Options

#### JSON Encoding/Decoding

Customize JSON marshaling and unmarshaling:

```go
import "encoding/json"

m.Initialize(
    // Custom JSON marshal function
    m.WithJSONMarshal(func(v any) ([]byte, error) {
        return json.MarshalIndent(v, "", "  ") // Pretty print
    }),
    
    // Custom JSON encode function (streaming)
    m.WithJSONEncode(func(w io.Writer, v any) error {
        encoder := json.NewEncoder(w)
        encoder.SetIndent("", "  ")
        return encoder.Encode(v)
    }),
    
    // Custom JSON unmarshal function
    m.WithJSONUnmarshal(json.Unmarshal),
)
```

#### Schema Decoder

Customize form and query parameter parsing:

```go
import "github.com/gorilla/schema"

decoder := schema.NewDecoder()
decoder.IgnoreUnknownKeys(true)
decoder.SetAliasTag("form")

m.Initialize(
    m.WithSchemaDecoder(decoder),
)
```

#### Logging

Provide a custom logger:

```go
import (
    "log"
    "os"
)

customLogger := log.New(os.Stdout, "[MINT] ", log.LstdFlags)

m.Initialize(
    m.WithLogger(customLogger),
)
```

#### Validation

Control validation behavior:

```go
import "github.com/go-playground/validator/v10"

// Disable validation
m.Initialize(
    m.WithValidation(false),
)

// Or use custom validator
v := validator.New()
v.RegisterValidation("customrule", myValidationFunc)

m.Initialize(
    m.WithValidator(v),
)
```

#### Error Handling

Customize error response format:

```go
m.Initialize(
    m.WithErrorHandler(func(w http.ResponseWriter, err error) {
        // Custom error logging
        log.Printf("[ERROR] %v", err)
        
        // Custom error response
        w.Header().Set("Content-Type", "application/json")
        w.WriteHeader(500)
        json.NewEncoder(w).Encode(map[string]string{
            "error": err.Error(),
            "timestamp": time.Now().Format(time.RFC3339),
        })
    }),
)
```

### Configuration Methods

#### `Initialize(opts ...Option)`

**One-time setup** at application startup. Uses `sync.Once` internally - safe to call multiple times but only the first call takes effect:

```go
func main() {
    m.Initialize(
        m.WithLogger(customLogger),
        m.WithValidation(true),
    )
    
    // Start your server...
}
```

#### `Configure(opts ...Option)`

**Runtime configuration updates**. Can be called multiple times to modify settings after initialization:

```go
// Enable debug mode at runtime
m.Configure(
    m.WithJSONMarshal(func(v any) ([]byte, error) {
        return json.MarshalIndent(v, "", "  ")
    }),
)
```

#### `Reset()`

**Reset to defaults** - useful for testing:

```go
func TestSomething(t *testing.T) {
    defer m.Reset() // Restore defaults after test
    
    m.Configure(m.WithValidation(false))
    // Test code...
}
```

### Complete Configuration Example

```go
package main

import (
    "encoding/json"
    "io"
    "log"
    "net/http"
    "os"
    "time"
    
    "github.com/cymoo/mint"
    "github.com/go-playground/validator/v10"
    "github.com/gorilla/schema"
)

func main() {
    // Configure logger
    logger := log.New(os.Stdout, "[API] ", log.LstdFlags|log.Lshortfile)
    
    // Configure schema decoder
    decoder := schema.NewDecoder()
    decoder.IgnoreUnknownKeys(true)
    decoder.SetAliasTag("form")
    
    // Configure validator
    v := validator.New()
    v.RegisterValidation("username", func(fl validator.FieldLevel) bool {
        return len(fl.Field().String()) >= 3
    })
    
    // Initialize framework
    m.Initialize(
        m.WithLogger(logger),
        m.WithSchemaDecoder(decoder),
        m.WithValidator(v),
        m.WithJSONMarshal(func(v any) ([]byte, error) {
            return json.MarshalIndent(v, "", "  ")
        }),
        m.WithErrorHandler(func(w http.ResponseWriter, err error) {
            logger.Printf("Error occurred: %v", err)
            
            response := map[string]any{
                "success": false,
                "error":   err.Error(),
                "time":    time.Now().Unix(),
            }
            
            w.Header().Set("Content-Type", "application/json")
            w.WriteHeader(http.StatusInternalServerError)
            json.NewEncoder(w).Encode(response)
        }),
    )
    
    // Setup routes
    mux := http.NewServeMux()
    // ... your routes
    
    logger.Println("Server starting on :8080")
    log.Fatal(http.ListenAndServe(":8080", mux))
}
```

### Thread Safety

All configuration methods are thread-safe:
- `Initialize()` uses `sync.Once` for one-time setup
- `Configure()` and `Reset()` use mutex locks for safe concurrent access
- Config reads use `RWMutex` for efficient concurrent access

### Default Configuration

If you don't call `Initialize()` or `Configure()`, Mint uses sensible defaults:

```go
// Default config (automatically applied)
{
    SchemaDecoder:     schema.NewDecoder() with IgnoreUnknownKeys(true),
    EnableValidation:  true,
    Validator:         validator with JSON/form tag support,
    Logger:            log.Default(),
    JSONMarshalFunc:   json.Marshal,
    JSONUnmarshalFunc: json.Unmarshal,
}
```

## 🎯 Complete Example

```go
package main

import (
    "log"
    "net/http"
    "github.com/cymoo/mint"
)

type User struct {
    ID    int    `json:"id"`
    Name  string `json:"name"`
    Email string `json:"email"`
}

type CreateUserRequest struct {
    Name  string `json:"name"`
    Email string `json:"email"`
}

var users = map[int]User{
    1: {ID: 1, Name: "Alice", Email: "alice@example.com"},
}

func main() {
    mux := http.NewServeMux()
    
    // List users
    mux.HandleFunc("GET /api/users", m.H(func() []User {
        result := make([]User, 0, len(users))
        for _, u := range users {
            result = append(result, u)
        }
        return result
    }))
    
    // Get user by ID
    mux.HandleFunc("GET /api/users/{id}", m.H(func(id m.Path[int]) (User, error) {
        user, ok := users[id.Value]
        if !ok {
            return User{}, &m.HTTPError{Code: 404, Err: "not_found"}
        }
        return user, nil
    }))
    
    // Create user
    mux.HandleFunc("POST /api/users", m.H(func(body m.JSON[CreateUserRequest]) m.Result[User] {
        user := User{
            ID:    len(users) + 1,
            Name:  body.Value.Name,
            Email: body.Value.Email,
        }
        users[user.ID] = user
        
        return m.Result[User]{
            Code: 201,
            Data: user,
        }
    }))
    
    // Delete user
    mux.HandleFunc("DELETE /api/users/{id}", m.H(func(id m.Path[int]) (m.StatusCode, error) {
        if _, ok := users[id.Value]; !ok {
            return 0, &m.HTTPError{Code: 404, Err: "not_found"}
        }
        delete(users, id.Value)
        return m.StatusCode(204), nil
    }))
    
    log.Println("Server running on :8080")
    log.Fatal(http.ListenAndServe(":8080", mux))
}
```

## 🔍 Error Response Format

Errors are automatically serialized to JSON:

```json
{
  "code": 404,
  "error": "not_found",
  "message": "user not found"
}
```

### Built-in Error Types

The framework handles common errors automatically:

- `json.UnmarshalTypeError` → 400 with field details
- `json.SyntaxError` → 400 invalid JSON
- `schema.MultiError` → 400 with validation messages
- Generic errors → Status inferred from message (e.g., "not found" → 404)

## 🎨 Best Practices

### 1. Use Descriptive Error Messages

```go
return &m.HTTPError{
    Code:    400,
    Err:     "validation_error",
    Message: "email must contain @ symbol",
}
```

### 2. Leverage Type Safety

```go
// Good: Type-safe path parameter
func getUser(id m.Path[int]) (User, error)

// Avoid: Manual parsing
func getUser(r *http.Request) (User, error) {
    idStr := r.PathValue("id")
    id, _ := strconv.Atoi(idStr) // Error-prone
}
```

### 3. Return Structs for JSON

```go
// Good: Automatic JSON serialization
func listUsers() []User

// Verbose: Manual serialization
func listUsers(w http.ResponseWriter, r *http.Request) {
    json.NewEncoder(w).Encode(users)
}
```

### 4. Combine Extractors

```go
// Multiple parameters work seamlessly
func updateUser(
    id m.Path[int],
    body m.JSON[UpdateUserRequest],
    q m.Query[Options],
) (User, error) {
    // Handler implementation
}
```

## 🤝 Contributing

Contributions are welcome! Please feel free to submit a Pull Request.

## 📄 License

MIT License - see [LICENSE](LICENSE) file for details

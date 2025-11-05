package m

import (
	"errors"
	"net/http/httptest"
	"testing"
)

// TestResultEdgeCases tests edge cases for Result handling
func TestResultEdgeCases(t *testing.T) {
	Reset()
	
	t.Run("Result with zero Code should default to 200", func(t *testing.T) {
		handler := H(func() Result[string] {
			return Result[string]{
				Code: 0,  // Zero value
				Data: "test data",
			}
		})

		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		handler(rec, req)

		// Should default to 200 OK
		if rec.Code != 200 {
			t.Errorf("expected HTTP status 200, got %d", rec.Code)
		}
	})
	
	t.Run("Result with negative Code should be handled", func(t *testing.T) {
		handler := H(func() Result[string] {
			return Result[string]{
				Code: -1,  // Invalid negative code
				Data: "test",
			}
		})

		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		handler(rec, req)

		t.Logf("Response Code: %d", rec.Code)
		// Negative codes should be handled - ResponseWriter will convert to 200
	})
	
	t.Run("Result with both Data and Err - should prioritize Err", func(t *testing.T) {
		handler := H(func() Result[string] {
			return Result[string]{
				Code: 500,
				Data: "this should not appear",
				Err:  errors.New("error occurred"),
			}
		})

		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		handler(rec, req)

		if rec.Code != 500 {
			t.Errorf("expected HTTP status 500, got %d", rec.Code)
		}
		
		// Should return error, not data
		if rec.Body.String() == "this should not appear" {
			t.Error("should not return data when error is set")
		}
		
		// Should contain error JSON
		var httpErr HTTPError
		parseJSONResponse(t, rec.Body.Bytes(), &httpErr)
		if httpErr.Code != 500 {
			t.Errorf("expected error code 500 in JSON, got %d", httpErr.Code)
		}
	})
}

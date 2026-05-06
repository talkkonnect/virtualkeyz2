package app

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandleAdminPage(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	rec := httptest.NewRecorder()
	handleAdminPage(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	if body == "" {
		t.Fatal("empty body")
	}
}

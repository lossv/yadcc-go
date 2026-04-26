package cache

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestServerEntryPutGetAndStats(t *testing.T) {
	store := NewMemoryStore()
	putReq := httptest.NewRequest(http.MethodPut, "/cache/entry?key=k", strings.NewReader("value"))
	putRec := httptest.NewRecorder()
	handleEntry(putRec, putReq, store)
	if putRec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d", putRec.Code)
	}

	getReq := httptest.NewRequest(http.MethodGet, "/cache/entry?key=k", nil)
	getRec := httptest.NewRecorder()
	handleEntry(getRec, getReq, store)
	body, _ := io.ReadAll(getRec.Result().Body)
	if getRec.Code != http.StatusOK || string(body) != "value" {
		t.Fatalf("GET status/body = %d/%q", getRec.Code, body)
	}

	statsReq := httptest.NewRequest(http.MethodGet, "/cache/stats", nil)
	statsRec := httptest.NewRecorder()
	handleStats(statsRec, statsReq, store)
	body, _ = io.ReadAll(statsRec.Result().Body)
	if !strings.Contains(string(body), `"entries":1`) {
		t.Fatalf("stats body = %s", body)
	}
}

func TestServerBloom(t *testing.T) {
	store := NewMemoryStore()
	if err := store.Put("k", []byte("value")); err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/cache/bloom", nil)
	rec := httptest.NewRecorder()

	handleBloom(rec, req, store)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if rec.Header().Get("X-Yadcc-Bloom-Hashes") == "" {
		t.Fatal("missing bloom hashes header")
	}
	if rec.Body.Len() == 0 {
		t.Fatal("empty bloom body")
	}
}

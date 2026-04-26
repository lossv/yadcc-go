package client

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"yadcc-go/internal/compiler"
)

// ---------- daemonBaseURL ----------

func TestDaemonBaseURL_default(t *testing.T) {
	os.Unsetenv("YADCC_DAEMON_ADDR")
	got := daemonBaseURL()
	if got != "http://127.0.0.1:8334" {
		t.Errorf("default URL = %q, want http://127.0.0.1:8334", got)
	}
}

func TestDaemonBaseURL_envOverride(t *testing.T) {
	os.Setenv("YADCC_DAEMON_ADDR", "http://10.0.0.1:9999")
	t.Cleanup(func() { os.Unsetenv("YADCC_DAEMON_ADDR") })
	got := daemonBaseURL()
	if got != "http://10.0.0.1:9999" {
		t.Errorf("overridden URL = %q, want http://10.0.0.1:9999", got)
	}
}

// ---------- isDirectInvocation ----------

func TestIsDirectInvocation(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"yadcc", true},
		{"yadcc-go", true},
		{"yadcc-cxx", true},
		{"gcc", false},
		{"g++", false},
		{"clang", false},
	}
	for _, tc := range cases {
		if got := isDirectInvocation(tc.name); got != tc.want {
			t.Errorf("isDirectInvocation(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// ---------- submitToDaemon (via httptest) ----------

func TestSubmitToDaemon_success(t *testing.T) {
	want := SubmitResponse{ExitCode: 0, ObjectFile: []byte("fake-obj"), CacheHit: true}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/local/submit_task" {
			http.NotFound(w, r)
			return
		}
		var req SubmitRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		json.NewEncoder(w).Encode(want) //nolint:errcheck
	}))
	defer srv.Close()

	os.Setenv("YADCC_DAEMON_ADDR", srv.URL)
	t.Cleanup(func() { os.Unsetenv("YADCC_DAEMON_ADDR") })

	pp := compiler.PreprocessResult{Language: "c", Source: []byte("int main(){}")}
	got, err := submitToDaemon("/usr/bin/gcc", []string{"-c", "foo.c"}, pp, "foo.o")
	if err != nil {
		t.Fatalf("submitToDaemon error: %v", err)
	}
	if got.ExitCode != want.ExitCode {
		t.Errorf("ExitCode = %d, want %d", got.ExitCode, want.ExitCode)
	}
	if string(got.ObjectFile) != string(want.ObjectFile) {
		t.Errorf("ObjectFile = %q, want %q", got.ObjectFile, want.ObjectFile)
	}
	if !got.CacheHit {
		t.Error("expected CacheHit=true")
	}
}

func TestSubmitToDaemon_nonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	os.Setenv("YADCC_DAEMON_ADDR", srv.URL)
	t.Cleanup(func() { os.Unsetenv("YADCC_DAEMON_ADDR") })

	pp := compiler.PreprocessResult{Language: "c", Source: []byte("int x;")}
	_, err := submitToDaemon("/usr/bin/gcc", nil, pp, "out.o")
	if err == nil {
		t.Fatal("expected error on non-200 response")
	}
}

func TestSubmitToDaemon_badJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("not-json")) //nolint:errcheck
	}))
	defer srv.Close()

	os.Setenv("YADCC_DAEMON_ADDR", srv.URL)
	t.Cleanup(func() { os.Unsetenv("YADCC_DAEMON_ADDR") })

	pp := compiler.PreprocessResult{Language: "c", Source: []byte("int x;")}
	_, err := submitToDaemon("/usr/bin/gcc", nil, pp, "out.o")
	if err == nil {
		t.Fatal("expected error on malformed JSON response")
	}
}

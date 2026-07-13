package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/njdaniel/conch/pkg/schema"
)

func TestSendServerFlagOverridesEnvironment(t *testing.T) {
	var called bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(schema.PostMessageResponse{})
	}))
	defer server.Close()
	t.Setenv("CONCH_SERVER", "http://127.0.0.1:1")
	t.Setenv("CONCH_AUTHOR", "9")

	var stdout, stderr bytes.Buffer
	err := Run(context.Background(), []string{"send", "--server", server.URL, "general", "hello"}, &stdout, &stderr, "test")
	if err != nil {
		t.Fatalf("run send: %v", err)
	}
	if !called {
		t.Fatal("flag server was not called")
	}
}

func TestSendUsesServerEnvironment(t *testing.T) {
	var called bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(schema.PostMessageResponse{})
	}))
	defer server.Close()
	t.Setenv("CONCH_SERVER", server.URL)
	t.Setenv("CONCH_AUTHOR", "9")

	var stdout, stderr bytes.Buffer
	err := Run(context.Background(), []string{"send", "general", "hello"}, &stdout, &stderr, "test")
	if err != nil {
		t.Fatalf("run send: %v", err)
	}
	if !called {
		t.Fatal("environment server was not called")
	}
}

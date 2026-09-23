package main

import (
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBuildEventContentSuccess(t *testing.T) {
	result := &LoadResult{Values: map[string]string{"DB_PASSWORD": "x", "API_KEY": "y"}}
	reason, eventType, message := buildEventContent(result)

	if reason != "SecretsLoaded" || eventType != "Normal" {
		t.Fatalf("got reason=%q type=%q", reason, eventType)
	}
	if !strings.Contains(message, "found 2 keys") ||
		!strings.Contains(message, "API_KEY") ||
		!strings.Contains(message, "DB_PASSWORD") {
		t.Fatalf("unexpected message: %q", message)
	}
}

func TestBuildEventContentPartialMissing(t *testing.T) {
	result := &LoadResult{
		Values:  map[string]string{"DB_PASSWORD": "x", "API_KEY": "y"},
		Missing: []string{"/vault/secrets/extra.json"},
	}
	reason, eventType, message := buildEventContent(result)

	if reason != "SecretsLoaded" || eventType != "Normal" {
		t.Fatalf("got reason=%q type=%q", reason, eventType)
	}
	want := "found 2 keys (API_KEY, DB_PASSWORD); no match for: /vault/secrets/extra.json"
	if message != want {
		t.Fatalf("got %q, want %q", message, want)
	}
}

func TestBuildEventContentNotFound(t *testing.T) {
	result := &LoadResult{Values: map[string]string{}, Searched: []string{"/vault/secrets/config", "/vault/secrets/*.json"}}
	reason, eventType, message := buildEventContent(result)

	if reason != "SecretsNotFound" || eventType != "Warning" {
		t.Fatalf("got reason=%q type=%q", reason, eventType)
	}
	if !strings.Contains(message, "/vault/secrets/config") || !strings.Contains(message, "/vault/secrets/*.json") {
		t.Fatalf("unexpected message: %q", message)
	}
}

func TestTruncateKeyListNoTruncationNeeded(t *testing.T) {
	keys := []string{"A", "B", "C"}
	got := truncateKeyList(keys)
	if got != "A, B, C" {
		t.Fatalf("got %q", got)
	}
}

func TestTruncateKeyListTruncatesOnBoundary(t *testing.T) {
	var keys []string
	for i := 0; i < 200; i++ {
		keys = append(keys, fmt.Sprintf("VERY_LONG_SECRET_KEY_NAME_NUMBER_%03d", i))
	}

	got := truncateKeyList(keys)

	if len(got) > maxEventMessage {
		t.Fatalf("truncated list still exceeds budget: %d bytes", len(got))
	}
	if !strings.Contains(got, "... and ") || !strings.Contains(got, " more") {
		t.Fatalf("expected truncation suffix, got %q", got)
	}
	// Must never cut mid-name: every comma-separated segment before the
	// suffix must be one of the original keys in full.
	body := strings.SplitN(got, "... and", 2)[0]
	body = strings.TrimSuffix(strings.TrimSpace(body), ",")
	for _, part := range strings.Split(body, ", ") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		found := false
		for _, k := range keys {
			if k == part {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("truncated output contains a partial/unknown key: %q", part)
		}
	}
}

func TestResolvePodIdentity(t *testing.T) {
	dir := t.TempDir()
	nsPath := filepath.Join(dir, "namespace")

	t.Run("success", func(t *testing.T) {
		if err := os.WriteFile(nsPath, []byte("my-namespace\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("POD_UID", "abc-123")

		hostname := func() (string, error) { return "my-pod", nil }
		name, namespace, uid, err := resolvePodIdentity(hostname, nsPath)
		if err != nil {
			t.Fatal(err)
		}
		if name != "my-pod" || namespace != "my-namespace" || uid != "abc-123" {
			t.Fatalf("got name=%q namespace=%q uid=%q", name, namespace, uid)
		}
	})

	t.Run("missing POD_UID skips", func(t *testing.T) {
		if err := os.WriteFile(nsPath, []byte("my-namespace\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("POD_UID", "")

		hostname := func() (string, error) { return "my-pod", nil }
		if _, _, _, err := resolvePodIdentity(hostname, nsPath); err == nil {
			t.Fatal("expected error when POD_UID is unset")
		}
	})

	t.Run("missing namespace file skips", func(t *testing.T) {
		t.Setenv("POD_UID", "abc-123")
		hostname := func() (string, error) { return "my-pod", nil }
		if _, _, _, err := resolvePodIdentity(hostname, filepath.Join(dir, "does-not-exist")); err == nil {
			t.Fatal("expected error when namespace file is missing")
		}
	})
}

func TestBuildEventBodyShape(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	body := buildEventBody("my-ns", "my-pod", "uid-123", "SecretsLoaded", "Normal", "found 1 keys (FOO)", now)

	var decoded eventBody
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}

	if decoded.APIVersion != "v1" || decoded.Kind != "Event" {
		t.Fatalf("got apiVersion=%q kind=%q", decoded.APIVersion, decoded.Kind)
	}
	if decoded.Metadata.GenerateName != "secrets-entrypoint-" || decoded.Metadata.Namespace != "my-ns" {
		t.Fatalf("got metadata=%+v", decoded.Metadata)
	}
	if decoded.InvolvedObject.Name != "my-pod" || decoded.InvolvedObject.UID != "uid-123" || decoded.InvolvedObject.Namespace != "my-ns" {
		t.Fatalf("got involvedObject=%+v", decoded.InvolvedObject)
	}
	if decoded.Reason != "SecretsLoaded" || decoded.Type != "Normal" {
		t.Fatalf("got reason=%q type=%q", decoded.Reason, decoded.Type)
	}
	if decoded.Source.Component != "secrets-entrypoint" {
		t.Fatalf("got source=%+v", decoded.Source)
	}
	if decoded.FirstTimestamp != decoded.LastTimestamp || decoded.Count != 1 {
		t.Fatalf("got firstTimestamp=%q lastTimestamp=%q count=%d", decoded.FirstTimestamp, decoded.LastTimestamp, decoded.Count)
	}
}

func TestPostEventRequest(t *testing.T) {
	var receivedAuth, receivedMethod, receivedPath string
	var receivedBody eventBody

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedMethod = r.Method
		receivedPath = r.URL.Path
		receivedAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&receivedBody); err != nil {
			t.Errorf("server: decoding body: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	client, err := newEventHTTPClient(caPEM)
	if err != nil {
		t.Fatal(err)
	}

	body := buildEventBody("default", "my-pod", "uid-123", "SecretsLoaded", "Normal", "found 1 keys (FOO)", time.Now().UTC())

	if err := postEventRequest(client, srv.URL, "default", "test-token", body); err != nil {
		t.Fatalf("postEventRequest failed: %v", err)
	}

	if receivedMethod != http.MethodPost {
		t.Fatalf("got method %q", receivedMethod)
	}
	if receivedPath != "/api/v1/namespaces/default/events" {
		t.Fatalf("got path %q", receivedPath)
	}
	if receivedAuth != "Bearer test-token" {
		t.Fatalf("got Authorization %q", receivedAuth)
	}
	if receivedBody.Reason != "SecretsLoaded" {
		t.Fatalf("got reason %q", receivedBody.Reason)
	}
}

func TestPostEventRequestNonSuccessStatus(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"forbidden"}`))
	}))
	defer srv.Close()

	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	client, err := newEventHTTPClient(caPEM)
	if err != nil {
		t.Fatal(err)
	}

	err = postEventRequest(client, srv.URL, "default", "test-token", []byte(`{}`))
	if err == nil {
		t.Fatal("expected error for 403 response")
	}
}

func TestNewEventHTTPClientInvalidCA(t *testing.T) {
	if _, err := newEventHTTPClient([]byte("not a cert")); err == nil {
		t.Fatal("expected error for invalid CA PEM")
	}
}

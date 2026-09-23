package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	saTokenPath     = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	saCACertPath    = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	saNamespacePath = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

	eventTimeout = 3 * time.Second

	// maxEventMessage mirrors the conventional client-go event message
	// budget (k8s.io/client-go/tools/record truncates at 1024 bytes).
	// We're not using client-go, but the same server-side expectations
	// apply, so the key list is truncated to stay comfortably under it.
	maxEventMessage = 1024
)

// ReportOutcome sends a best-effort Kubernetes Event describing the
// result of loading secrets. It never returns an error and never blocks
// beyond eventTimeout: every failure path is a single warning line on
// stderr, since Event delivery must never change the exec outcome.
func ReportOutcome(result *LoadResult) {
	token, err := os.ReadFile(saTokenPath)
	if err != nil {
		// No service account token mounted (e.g. automountServiceAccountToken:
		// false) is a normal, expected configuration -- skip silently.
		return
	}

	name, namespace, uid, err := resolvePodIdentity(os.Hostname, saNamespacePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "secrets-entrypoint: skipping Kubernetes event:", err)
		return
	}

	host := os.Getenv("KUBERNETES_SERVICE_HOST")
	port := os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		fmt.Fprintln(os.Stderr, "secrets-entrypoint: warning: KUBERNETES_SERVICE_HOST/PORT not set; skipping Kubernetes event")
		return
	}

	caCert, err := os.ReadFile(saCACertPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "secrets-entrypoint: warning: failed to send Kubernetes event:", err)
		return
	}
	client, err := newEventHTTPClient(caCert)
	if err != nil {
		fmt.Fprintln(os.Stderr, "secrets-entrypoint: warning: failed to send Kubernetes event:", err)
		return
	}

	reason, eventType, message := buildEventContent(result)
	body := buildEventBody(namespace, name, uid, reason, eventType, message, time.Now().UTC())

	apiBase := "https://" + net.JoinHostPort(host, port)
	if err := postEventRequest(client, apiBase, namespace, strings.TrimSpace(string(token)), body); err != nil {
		fmt.Fprintln(os.Stderr, "secrets-entrypoint: warning: failed to send Kubernetes event:", err)
	}
}

// resolvePodIdentity resolves the three pieces of pod identity the
// Events API needs. hostname is injected (rather than calling
// os.Hostname directly) purely for testability.
//
// KNOWN LIMITATION (do not silently work around): under hostNetwork:
// true, the container hostname is the *node's* hostname, not the pod
// name, so the reported involvedObject.name will be wrong. This is
// documented, not auto-detected.
func resolvePodIdentity(hostname func() (string, error), namespaceFilePath string) (name, namespace, uid string, err error) {
	name, err = hostname()
	if err != nil || name == "" {
		return "", "", "", fmt.Errorf("could not determine pod name from hostname: %w", err)
	}

	nsBytes, err := os.ReadFile(namespaceFilePath)
	if err != nil {
		return "", "", "", fmt.Errorf("could not read namespace file %s: %w", namespaceFilePath, err)
	}
	namespace = strings.TrimSpace(string(nsBytes))

	uid = os.Getenv("POD_UID")
	if uid == "" {
		return "", "", "", fmt.Errorf("POD_UID environment variable not set (inject it via the Downward API, fieldRef metadata.uid)")
	}

	return name, namespace, uid, nil
}

// buildEventContent decides the single event this run will report: a
// success (Normal/SecretsLoaded) event if at least one key was loaded
// anywhere -- additionally naming any configured entry that contributed
// zero files, so a partial miss isn't silently absorbed into an
// all-green message -- otherwise a not-found (Warning/SecretsNotFound)
// event listing every configured path/pattern that was searched.
func buildEventContent(result *LoadResult) (reason, eventType, message string) {
	if len(result.Values) > 0 {
		keys := sortedKeys(result.Values)
		message = fmt.Sprintf("found %d keys (%s)", len(keys), truncateKeyList(keys))

		if len(result.Missing) > 0 {
			const label = "; no match for: "
			if budget := maxEventMessage - len(message) - len(label); budget > 0 {
				message += label + truncateList(result.Missing, budget)
			}
		}
		return "SecretsLoaded", "Normal", message
	}
	return "SecretsNotFound", "Warning", fmt.Sprintf("no secrets found; searched: %s", strings.Join(result.Searched, ", "))
}

// truncateKeyList joins keys with ", ", truncating (on a whole-name
// boundary, never mid-name) and appending "... and N more" if the full
// list would push the event message over maxEventMessage.
func truncateKeyList(keys []string) string {
	return truncateList(keys, maxEventMessage-64) // headroom for "found N keys (...)" wrapper
}

// truncateList joins items with ", ", truncating on a whole-item
// boundary (never mid-item) and appending "... and N more" if the full
// join would exceed budget bytes.
func truncateList(items []string, budget int) string {
	joined := strings.Join(items, ", ")
	if len(joined) <= budget {
		return joined
	}

	var kept []string
	length := 0
	for i, it := range items {
		addition := len(it)
		if i > 0 {
			addition += 2 // ", "
		}
		if length+addition > budget {
			return strings.Join(kept, ", ") + fmt.Sprintf("... and %d more", len(items)-i)
		}
		kept = append(kept, it)
		length += addition
	}
	return strings.Join(kept, ", ")
}

type eventBody struct {
	APIVersion     string         `json:"apiVersion"`
	Kind           string         `json:"kind"`
	Metadata       eventMetadata  `json:"metadata"`
	InvolvedObject involvedObject `json:"involvedObject"`
	Reason         string         `json:"reason"`
	Message        string         `json:"message"`
	Type           string         `json:"type"`
	Source         eventSource    `json:"source"`
	FirstTimestamp string         `json:"firstTimestamp"`
	LastTimestamp  string         `json:"lastTimestamp"`
	Count          int            `json:"count"`
}

type eventMetadata struct {
	GenerateName string `json:"generateName"`
	Namespace    string `json:"namespace"`
}

type involvedObject struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Namespace  string `json:"namespace"`
	UID        string `json:"uid"`
}

type eventSource struct {
	Component string `json:"component"`
}

// buildEventBody renders the exact JSON shape validated against a live
// cluster in spec §9, using metadata.generateName (not a fixed name) so
// repeated pod restarts don't collide on the same Event object name.
func buildEventBody(namespace, podName, podUID, reason, eventType, message string, now time.Time) []byte {
	ts := now.Format(time.RFC3339)
	body := eventBody{
		APIVersion: "v1",
		Kind:       "Event",
		Metadata: eventMetadata{
			GenerateName: "secrets-entrypoint-",
			Namespace:    namespace,
		},
		InvolvedObject: involvedObject{
			APIVersion: "v1",
			Kind:       "Pod",
			Name:       podName,
			Namespace:  namespace,
			UID:        podUID,
		},
		Reason:         reason,
		Message:        message,
		Type:           eventType,
		Source:         eventSource{Component: "secrets-entrypoint"},
		FirstTimestamp: ts,
		LastTimestamp:  ts,
		Count:          1,
	}
	// Marshal of a fixed, always-valid struct never fails.
	out, _ := json.Marshal(body)
	return out
}

func newEventHTTPClient(caPEM []byte) (*http.Client, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("no valid certificates found in %s", saCACertPath)
	}
	return &http.Client{
		Timeout: eventTimeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool},
		},
	}, nil
}

func postEventRequest(client *http.Client, apiBase, namespace, token string, body []byte) error {
	// namespace comes from a trusted, cluster-mounted file, but escape it
	// anyway: cheap defense-in-depth, and it avoids a broken request if a
	// namespace value ever contains an unexpected character.
	endpoint := fmt.Sprintf("%s/api/v1/namespaces/%s/events", apiBase, url.PathEscape(namespace))

	ctx, cancel := context.WithTimeout(context.Background(), eventTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("unexpected status %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}

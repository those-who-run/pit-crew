package kube

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, name, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func env(vars map[string]string) func(string) string {
	return func(key string) string { return vars[key] }
}

// A fake API server over plain HTTP, so no CA is needed.
func newServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *Client) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := New(Config{APIURL: server.URL, TokenFile: write(t, "token", "sa-token\n"), Namespace: "twr-agents"}, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	return server, client
}

func TestNew(t *testing.T) {
	t.Run("outside a pod with nothing set", func(t *testing.T) {
		if _, err := New(Config{}, env(nil)); err == nil || !strings.Contains(err.Error(), "POLL_KUBE_API") {
			t.Errorf("error %v", err)
		}
	})
	t.Run("in a pod, the service account files are missing here", func(t *testing.T) {
		_, err := New(Config{}, env(map[string]string{"KUBERNETES_SERVICE_HOST": "192.0.2.1", "KUBERNETES_SERVICE_PORT": "443"}))
		if err == nil || !strings.Contains(err.Error(), "token") {
			t.Errorf("error %v", err)
		}
	})
	t.Run("an IPv6 host", func(t *testing.T) {
		if got := net("fd00::1", "443"); got != "[fd00::1]:443" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("https needs a CA", func(t *testing.T) {
		_, err := New(Config{APIURL: "https://192.0.2.1", TokenFile: write(t, "token", "x"), Namespace: "n", CAFile: filepath.Join(t.TempDir(), "none")}, env(nil))
		if err == nil || !strings.Contains(err.Error(), "CA") {
			t.Errorf("error %v", err)
		}
		_, err = New(Config{APIURL: "https://192.0.2.1", TokenFile: write(t, "token", "x"), Namespace: "n", CAFile: write(t, "ca", "not a certificate")}, env(nil))
		if err == nil || !strings.Contains(err.Error(), "no certificate") {
			t.Errorf("error %v", err)
		}
	})
	t.Run("the namespace comes from the configuration", func(t *testing.T) {
		client, err := New(Config{APIURL: "http://192.0.2.1", TokenFile: write(t, "token", "x"), Namespace: "twr-agents"}, env(nil))
		if err != nil || client.Namespace() != "twr-agents" {
			t.Errorf("%v %v", client, err)
		}
	})
}

const jobs = `{"items": [
  {"metadata": {"name": "review-ecu-31-1f0c5d2e-f0", "labels": {"app.kubernetes.io/managed-by": "pit-crew", "app.kubernetes.io/component": "review"},
    "annotations": {"pit-crew/repo": "those-who-run/ecu"}, "creationTimestamp": "2026-11-06T09:00:00Z"},
   "status": {"active": 1, "conditions": []}},
  {"metadata": {"name": "build-ecu-232", "labels": {"app.kubernetes.io/managed-by": "pit-crew"}, "creationTimestamp": "2026-11-06T08:00:00Z"},
   "status": {"conditions": [{"type": "Failed", "status": "True", "lastTransitionTime": "2026-11-06T08:20:00Z"}]}},
  {"metadata": {"name": "fix-ecu-31-r1", "labels": {"app.kubernetes.io/managed-by": "pit-crew"}, "creationTimestamp": "2026-11-05T08:00:00Z"},
   "status": {"conditions": [{"type": "Complete", "status": "True", "lastTransitionTime": "2026-11-05T08:30:00Z"}]}}
]}`

func TestListJobs(t *testing.T) {
	var seen string
	_, client := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		seen = r.Method + " " + r.URL.RequestURI() + " " + r.Header.Get("Authorization")
		_, _ = fmt.Fprint(w, jobs)
	})
	got, err := client.ListJobs(t.Context(), "app.kubernetes.io/managed-by=pit-crew")
	if err != nil {
		t.Fatal(err)
	}
	if seen != "GET /apis/batch/v1/namespaces/twr-agents/jobs?labelSelector=app.kubernetes.io%2Fmanaged-by%3Dpit-crew Bearer sa-token" {
		t.Errorf("request %q", seen)
	}
	if len(got) != 3 || !got[0].Active || got[0].Failed || got[0].Annotations["pit-crew/repo"] != "those-who-run/ecu" {
		t.Errorf("first %+v", got[0])
	}
	if got[1].Active || !got[1].Failed || !got[1].FinishedAt.Equal(time.Date(2026, 11, 6, 8, 20, 0, 0, time.UTC)) {
		t.Errorf("second %+v", got[1])
	}
	if got[2].Active || got[2].Failed || got[2].Labels["app.kubernetes.io/managed-by"] != "pit-crew" {
		t.Errorf("third %+v", got[2])
	}
}

func TestCreateJob(t *testing.T) {
	var body string
	_, client := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		body = r.Method + " " + r.URL.Path + " " + r.Header.Get("Content-Type") + " " + string(data)
		if strings.Contains(string(data), "exists") {
			w.WriteHeader(http.StatusConflict)
			_, _ = fmt.Fprint(w, `{"kind": "Status", "status": "Failure", "message": "jobs.batch \"exists\" already exists", "reason": "AlreadyExists", "code": 409}`)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprint(w, `{"kind": "Job"}`)
	})
	if err := client.CreateJob(t.Context(), []byte(`{"metadata": {"name": "a", "namespace": "twr-agents"}}`)); err != nil {
		t.Fatal(err)
	}
	if body != `POST /apis/batch/v1/namespaces/twr-agents/jobs application/json {"metadata": {"name": "a", "namespace": "twr-agents"}}` {
		t.Errorf("request %q", body)
	}
	err := client.CreateJob(t.Context(), []byte(`{"metadata": {"name": "exists"}}`))
	if !IsAlreadyExists(err) || !strings.Contains(err.Error(), "409 AlreadyExists") {
		t.Errorf("error %v, want already exists", err)
	}
	if err := client.CreateJob(t.Context(), []byte(`{"metadata": {"name": "a", "namespace": "other"}}`)); err == nil || !strings.Contains(err.Error(), "namespace") {
		t.Errorf("error %v, want the namespace refused", err)
	}
	if err := client.CreateJob(t.Context(), []byte(`nope`)); err == nil || !strings.Contains(err.Error(), "not JSON") {
		t.Errorf("error %v", err)
	}
	if IsAlreadyExists(fmt.Errorf("other")) {
		t.Error("a plain error is not already-exists")
	}
}

func TestContainerExit(t *testing.T) {
	pods := `{"items": [{"status": {
	  "initContainerStatuses": [
	    {"name": "fetch", "state": {"terminated": {"exitCode": 0}}},
	    {"name": "agent", "state": {"terminated": {"exitCode": 75}}}],
	  "containerStatuses": [{"name": "publish", "state": {"waiting": {"reason": "PodInitializing"}}}]}}]}`
	var seen string
	_, client := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.RequestURI()
		if strings.Contains(seen, "none") {
			_, _ = fmt.Fprint(w, `{"items": []}`)
			return
		}
		_, _ = fmt.Fprint(w, pods)
	})
	code, ok, err := client.ContainerExit(t.Context(), "review-ecu-31", "agent")
	if err != nil || !ok || code != 75 {
		t.Errorf("%d %v %v", code, ok, err)
	}
	if seen != "/api/v1/namespaces/twr-agents/pods?labelSelector=batch.kubernetes.io%2Fjob-name%3Dreview-ecu-31" {
		t.Errorf("request %q", seen)
	}
	if _, ok, err := client.ContainerExit(t.Context(), "review-ecu-31", "publish"); err != nil || ok {
		t.Errorf("a waiting container reported %v %v", ok, err)
	}
	if _, ok, err := client.ContainerExit(t.Context(), "none", "agent"); err != nil || ok {
		t.Errorf("no pod reported %v %v", ok, err)
	}
}

func TestErrors(t *testing.T) {
	_, client := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprint(w, `{"reason": "Forbidden", "message": "jobs is forbidden"}`)
	})
	if _, err := client.ListJobs(t.Context(), "x"); err == nil || !strings.Contains(err.Error(), "403 Forbidden: jobs is forbidden") {
		t.Errorf("error %v", err)
	}
	server, client := newServer(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, "not json") })
	if _, err := client.ListJobs(t.Context(), "x"); err == nil {
		t.Error("no error for a body that is not JSON")
	}
	server.Close()
	if _, err := client.ListJobs(t.Context(), "x"); err == nil {
		t.Error("no error with the server gone")
	}
}

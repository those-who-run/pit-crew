// Package kube is the sliver of the Kubernetes API the poller uses: create a Job,
// list the Jobs it made, and read the exit code of a finished Job's agent container.
// It speaks the REST API with the standard library, as the pod's service account,
// and needs nothing else of the cluster.
package kube

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// The in-cluster defaults every pod has.
const (
	serviceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"
	tokenFile         = serviceAccountDir + "/token"
	caFile            = serviceAccountDir + "/ca.crt"
	namespaceFile     = serviceAccountDir + "/namespace"
)

// Config says how to reach the API server.
type Config struct {
	// APIURL is the server, such as https://192.0.2.1:443; empty means the one
	// KUBERNETES_SERVICE_HOST and KUBERNETES_SERVICE_PORT name.
	APIURL string
	// TokenFile is the bearer token's file; empty means the service account's.
	TokenFile string
	// CAFile is the server's CA; empty means the service account's.
	CAFile string
	// Namespace is where Jobs live; empty means the pod's own.
	Namespace string
}

// Client talks to one API server in one namespace.
type Client struct {
	api       string
	token     string
	namespace string
	http      *http.Client
}

// New reads the configuration and the service account files. getenv is os.Getenv
// outside the tests.
func New(cfg Config, getenv func(string) string) (*Client, error) {
	api := cfg.APIURL
	if api == "" {
		host, port := getenv("KUBERNETES_SERVICE_HOST"), getenv("KUBERNETES_SERVICE_PORT")
		if host == "" || port == "" {
			return nil, errors.New("kube: not in a pod and no POLL_KUBE_API set")
		}
		api = "https://" + net(host, port)
	}
	token, err := readFile(cfg.TokenFile, tokenFile)
	if err != nil {
		return nil, fmt.Errorf("kube: token: %w", err)
	}
	namespace := cfg.Namespace
	if namespace == "" {
		if namespace, err = readFile("", namespaceFile); err != nil {
			return nil, fmt.Errorf("kube: namespace: %w", err)
		}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if ca, err := readFile(cfg.CAFile, caFile); err == nil {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(ca)) {
			return nil, errors.New("kube: the CA file holds no certificate")
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	} else if strings.HasPrefix(api, "https://") {
		return nil, fmt.Errorf("kube: CA: %w", err)
	}
	return &Client{api: strings.TrimRight(api, "/"), token: strings.TrimSpace(token), namespace: strings.TrimSpace(namespace),
		http: &http.Client{Transport: transport, Timeout: 30 * time.Second}}, nil
}

func net(host, port string) string {
	if strings.Contains(host, ":") {
		return "[" + host + "]:" + port
	}
	return host + ":" + port
}

func readFile(path, fallback string) (string, error) {
	if path == "" {
		path = fallback
	}
	data, err := os.ReadFile(path) //nolint:gosec // the path is the pod's service account mount or the operator's configuration
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// Namespace is where the client creates and lists Jobs.
func (c *Client) Namespace() string { return c.namespace }

// Error is an API response outside 2xx.
type Error struct {
	Status  int
	Reason  string
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("kube: %d %s: %s", e.Status, e.Reason, e.Message) }

// IsAlreadyExists reports whether err is the API refusing a create because the
// object is there.
func IsAlreadyExists(err error) bool {
	var kErr *Error
	return errors.As(err, &kErr) && kErr.Status == http.StatusConflict && kErr.Reason == "AlreadyExists"
}

func (c *Client) do(ctx context.Context, method, path string, body []byte, into any) error {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.api+path, reader)
	if err != nil {
		return fmt.Errorf("kube: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("kube: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return fmt.Errorf("kube: read %s: %w", path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		status := &Error{Status: resp.StatusCode}
		_ = json.Unmarshal(data, status)
		return status
	}
	if into != nil {
		if err := json.Unmarshal(data, into); err != nil {
			return fmt.Errorf("kube: %s: %w", path, err)
		}
	}
	return nil
}

// Job is what the poller reads about a Job it made.
type Job struct {
	Name        string
	Labels      map[string]string
	Annotations map[string]string
	CreatedAt   time.Time
	// Active is true until the Job has a Complete or Failed condition.
	Active bool
	Failed bool
	// FinishedAt is when the condition was set.
	FinishedAt time.Time
}

type jobJSON struct {
	Metadata struct {
		Name              string            `json:"name"`
		Labels            map[string]string `json:"labels"`
		Annotations       map[string]string `json:"annotations"`
		CreationTimestamp time.Time         `json:"creationTimestamp"`
	} `json:"metadata"`
	Status struct {
		Conditions []struct {
			Type               string    `json:"type"`
			Status             string    `json:"status"`
			LastTransitionTime time.Time `json:"lastTransitionTime"`
		} `json:"conditions"`
	} `json:"status"`
}

func (j jobJSON) job() Job {
	job := Job{Name: j.Metadata.Name, Labels: j.Metadata.Labels, Annotations: j.Metadata.Annotations, CreatedAt: j.Metadata.CreationTimestamp, Active: true}
	for _, c := range j.Status.Conditions {
		if c.Status != "True" {
			continue
		}
		switch c.Type {
		case "Complete":
			job.Active, job.FinishedAt = false, c.LastTransitionTime
		case "Failed":
			job.Active, job.Failed, job.FinishedAt = false, true, c.LastTransitionTime
		}
	}
	return job
}

// ListJobs returns the Jobs in the namespace carrying the label selector, such as
// `app.kubernetes.io/managed-by=pit-crew`.
func (c *Client) ListJobs(ctx context.Context, selector string) ([]Job, error) {
	var list struct {
		Items []jobJSON `json:"items"`
	}
	path := "/apis/batch/v1/namespaces/" + url.PathEscape(c.namespace) + "/jobs?labelSelector=" + url.QueryEscape(selector)
	if err := c.do(ctx, http.MethodGet, path, nil, &list); err != nil {
		return nil, err
	}
	jobs := make([]Job, 0, len(list.Items))
	for _, item := range list.Items {
		jobs = append(jobs, item.job())
	}
	return jobs, nil
}

// CreateJob creates a Job from its JSON, which must name the client's namespace or
// none. It returns IsAlreadyExists when a Job of that name is there.
func (c *Client) CreateJob(ctx context.Context, job []byte) error {
	var meta struct {
		Metadata struct {
			Namespace string `json:"namespace"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(job, &meta); err != nil {
		return fmt.Errorf("kube: the Job is not JSON: %w", err)
	}
	if meta.Metadata.Namespace != "" && meta.Metadata.Namespace != c.namespace {
		return fmt.Errorf("kube: the Job names namespace %q; this poller works in %q", meta.Metadata.Namespace, c.namespace)
	}
	return c.do(ctx, http.MethodPost, "/apis/batch/v1/namespaces/"+url.PathEscape(c.namespace)+"/jobs", job, nil)
}

// ContainerExit returns the exit code of a named container in the Job's pod, and
// false when no pod of the Job has that container terminated.
func (c *Client) ContainerExit(ctx context.Context, jobName, container string) (int, bool, error) {
	var list struct {
		Items []struct {
			Status struct {
				InitContainerStatuses []containerStatus `json:"initContainerStatuses"`
				ContainerStatuses     []containerStatus `json:"containerStatuses"`
			} `json:"status"`
		} `json:"items"`
	}
	path := "/api/v1/namespaces/" + url.PathEscape(c.namespace) + "/pods?labelSelector=" + url.QueryEscape("batch.kubernetes.io/job-name="+jobName)
	if err := c.do(ctx, http.MethodGet, path, nil, &list); err != nil {
		return 0, false, err
	}
	for _, pod := range list.Items {
		for _, status := range append(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses...) {
			if status.Name == container && status.State.Terminated != nil {
				return status.State.Terminated.ExitCode, true, nil
			}
		}
	}
	return 0, false, nil
}

type containerStatus struct {
	Name  string `json:"name"`
	State struct {
		Terminated *struct {
			ExitCode int `json:"exitCode"`
		} `json:"terminated"`
	} `json:"state"`
}

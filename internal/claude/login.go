package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// SecretConfig says which Secret holds the token, and how to reach the cluster.
type SecretConfig struct {
	Namespace string
	Name      string
	Key       string
	// Context is the kubectl context; empty means kubectl's current one.
	Context string
	Kubectl string
	Claude  string
	// WarnAfter is the token age `status` warns at.
	WarnAfter time.Duration
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// SecretConfigFromEnv reads the maintainer's configuration.
func SecretConfigFromEnv(getenv func(string) string) (SecretConfig, error) {
	cfg := SecretConfig{
		Namespace: or(getenv("TWR_CLAUDE_NAMESPACE"), "twr-agents"),
		Name:      or(getenv("TWR_CLAUDE_SECRET"), "twr-claude-token"),
		Key:       or(getenv("TWR_CLAUDE_SECRET_KEY"), "token"),
		Context:   getenv("TWR_CLAUDE_KUBE_CONTEXT"),
		Kubectl:   or(getenv("TWR_CLAUDE_KUBECTL"), "kubectl"),
		Claude:    or(getenv("TWR_CLAUDE_BIN"), "claude"),
		WarnAfter: 330 * 24 * time.Hour,
	}
	if v := getenv("TWR_CLAUDE_WARN_AFTER"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return cfg, fmt.Errorf("TWR_CLAUDE_WARN_AFTER: %q is not a duration", v)
		}
		cfg.WarnAfter = d
	}
	return cfg, nil
}

// IssuedAtAnnotation records when the token was made.
const IssuedAtAnnotation = "pit-crew/issued-at"

// The shape of a `setup-token` token in the CLI's output.
var oauthTokenRE = regexp.MustCompile(`sk-ant-oat01-[A-Za-z0-9_-]{20,}`)

func (c SecretConfig) kubectl(ctx context.Context, args ...string) *exec.Cmd {
	all := []string{"--namespace", c.Namespace}
	if c.Context != "" {
		all = append(all, "--context", c.Context)
	}
	return exec.CommandContext(ctx, c.Kubectl, append(all, args...)...) //nolint:gosec // kubectl and its context are the maintainer's configuration
}

// Login makes the token and writes it into the Secret. With fromStdin false it
// runs `claude setup-token`, forwards that process's output to stderr with the
// token taken out, and keeps the token; with fromStdin true it reads the token
// from stdin instead. The token goes to kubectl's standard input and nowhere
// else: not the terminal, not a file, not the shell's history.
func Login(ctx context.Context, cfg SecretConfig, fromStdin bool, stdin io.Reader, stderr io.Writer) int {
	var token string
	if fromStdin {
		data, err := io.ReadAll(io.LimitReader(stdin, 1<<16))
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "twr-claude: read stdin: %v\n", err)
			return ExitFailed
		}
		token = strings.TrimSpace(string(data))
	} else {
		cmd := exec.CommandContext(ctx, cfg.Claude, "setup-token") //nolint:gosec // the CLI is the maintainer's configuration
		cmd.Stdin = stdin
		var out bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = stderr
		if err := cmd.Run(); err != nil {
			_, _ = fmt.Fprintf(stderr, "twr-claude: claude setup-token: %v\n", err)
			return ExitFailed
		}
		token = oauthTokenRE.FindString(out.String())
		// What the CLI said, less the token, so the maintainer sees any instruction.
		_, _ = fmt.Fprint(stderr, oauthTokenRE.ReplaceAllString(out.String(), "<token>"))
	}
	if token == "" {
		_, _ = fmt.Fprintln(stderr, "twr-claude: no token found; run `claude setup-token` yourself and pass it with --from-stdin")
		return ExitFailed
	}
	now := time.Now
	if cfg.Now != nil {
		now = cfg.Now
	}
	// A Secret that exists is replaced: the manifest goes through a pipe, in
	// memory, to `kubectl apply`.
	render := cfg.kubectl(ctx, "create", "secret", "generic", cfg.Name, "--from-file="+cfg.Key+"=/dev/stdin", "--dry-run=client", "-o", "json")
	render.Stdin = strings.NewReader(token)
	manifest, err := render.Output()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "twr-claude: render the Secret: %v\n", err)
		return ExitFailed
	}
	manifest, err = annotate(manifest, IssuedAtAnnotation, now().UTC().Format(time.RFC3339))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "twr-claude: %v\n", err)
		return ExitFailed
	}
	apply := cfg.kubectl(ctx, "apply", "-f", "-")
	apply.Stdin = bytes.NewReader(manifest)
	apply.Stdout = stderr
	apply.Stderr = stderr
	if err := apply.Run(); err != nil {
		_, _ = fmt.Fprintf(stderr, "twr-claude: apply the Secret: %v\n", err)
		return ExitFailed
	}
	_, _ = fmt.Fprintf(stderr, "twr-claude: Secret %s/%s written, key %s, issued %s\n", cfg.Namespace, cfg.Name, cfg.Key, now().UTC().Format("2006-01-02"))
	return ExitOK
}

// annotate adds an annotation to a Secret manifest.
func annotate(manifest []byte, key, value string) ([]byte, error) {
	var secret map[string]any
	if err := json.Unmarshal(manifest, &secret); err != nil {
		return nil, fmt.Errorf("the Secret kubectl rendered is not JSON: %w", err)
	}
	meta, _ := secret["metadata"].(map[string]any)
	if meta == nil {
		return nil, errors.New("the Secret kubectl rendered has no metadata")
	}
	annotations, _ := meta["annotations"].(map[string]any)
	if annotations == nil {
		annotations = map[string]any{}
	}
	annotations[key] = value
	meta["annotations"] = annotations
	return json.Marshal(secret)
}

// Status reads the Secret's metadata and says how old the token is, never what
// it is. It returns 0 when the Secret is there and fresh, 4 when it is older than
// WarnAfter, and 1 when it is missing.
func Status(ctx context.Context, cfg SecretConfig, stdout, stderr io.Writer) int {
	get := cfg.kubectl(ctx, "get", "secret", cfg.Name, "-o", "json")
	get.Stderr = stderr
	data, err := get.Output()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "twr-claude: Secret %s/%s: %v\n", cfg.Namespace, cfg.Name, err)
		return ExitFailed
	}
	var secret struct {
		Metadata struct {
			Annotations       map[string]string `json:"annotations"`
			CreationTimestamp time.Time         `json:"creationTimestamp"`
		} `json:"metadata"`
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(data, &secret); err != nil {
		_, _ = fmt.Fprintf(stderr, "twr-claude: read the Secret: %v\n", err)
		return ExitFailed
	}
	if _, ok := secret.Data[cfg.Key]; !ok {
		_, _ = fmt.Fprintf(stderr, "twr-claude: Secret %s/%s has no key %s\n", cfg.Namespace, cfg.Name, cfg.Key)
		return ExitFailed
	}
	now := time.Now
	if cfg.Now != nil {
		now = cfg.Now
	}
	issued := secret.Metadata.CreationTimestamp
	source := "created"
	if v, err := time.Parse(time.RFC3339, secret.Metadata.Annotations[IssuedAtAnnotation]); err == nil {
		issued, source = v, "issued"
	}
	age := now().Sub(issued)
	days := int(age.Hours() / 24)
	_, _ = fmt.Fprintf(stdout, "Secret %s/%s: key %s present, %s %s, %d days old\n", cfg.Namespace, cfg.Name, cfg.Key, source, issued.UTC().Format("2006-01-02"), days)
	if age > cfg.WarnAfter {
		_, _ = fmt.Fprintf(stdout, "The token is older than %d days: a setup-token token lasts a year, so run `twr-claude login` soon.\n", int(cfg.WarnAfter.Hours()/24))
		return 4
	}
	return ExitOK
}

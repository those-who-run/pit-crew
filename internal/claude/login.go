package claude

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
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

// The shape of a `setup-token` token in the CLI's output: tokenPrefix, then
// at least 20 more of the characters isTokenByte allows.
var oauthTokenRE = regexp.MustCompile(tokenPrefix + "[A-Za-z0-9_-]{20,}")

// tokenPrefix is what every setup-token token starts with.
const tokenPrefix = "sk-ant-oat01-" //nolint:gosec // the shape of a token, not one

func (c SecretConfig) kubectl(ctx context.Context, args ...string) *exec.Cmd {
	all := []string{"--namespace", c.Namespace}
	if c.Context != "" {
		all = append(all, "--context", c.Context)
	}
	return exec.CommandContext(ctx, c.Kubectl, append(all, args...)...) //nolint:gosec // kubectl and its context are the maintainer's configuration
}

// Login makes the token and writes it into the Secret. With fromStdin false it
// runs `claude setup-token`, forwards that process's output to stderr as it
// comes with the token taken out, and keeps the token; with fromStdin true it
// asks for the token and reads one line from stdin instead. The token goes to
// kubectl's standard input and nowhere else: not the terminal, not a file, not
// the shell's history.
func Login(ctx context.Context, cfg SecretConfig, fromStdin bool, stdin io.Reader, stderr io.Writer) int {
	var token string
	if fromStdin {
		_, _ = fmt.Fprintln(stderr, "twr-claude: paste the token from `claude setup-token`, then Enter")
		line, err := firstLine(io.LimitReader(stdin, 1<<16))
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "twr-claude: read stdin: %v\n", err)
			return ExitFailed
		}
		if line == "" {
			_, _ = fmt.Fprintln(stderr, "twr-claude: nothing on stdin")
			return ExitFailed
		}
		// The browser's authorisation code looks nothing like a token, and the
		// Secret must never hold it.
		if oauthTokenRE.FindString(line) != line {
			_, _ = fmt.Fprintln(stderr, "twr-claude: that is not a setup-token token (they start sk-ant-oat01-); the browser's code goes to `claude setup-token`'s own prompt, which then prints the token")
			return ExitFailed
		}
		token = line
	} else {
		cmd := exec.CommandContext(ctx, cfg.Claude, "setup-token") //nolint:gosec // the CLI is the maintainer's configuration
		cmd.Stdin = stdin
		// Whatever the CLI prints, on either stream, comes through one pipe and
		// reaches the terminal while it says it, less the token: on a headless
		// host it prints a sign-in URL, asks for a code and waits for it on
		// stdin, which passes through.
		pr, pw, err := os.Pipe()
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "twr-claude: claude setup-token: %v\n", err)
			return ExitFailed
		}
		defer func() { _ = pr.Close() }()
		cmd.Stdout, cmd.Stderr = pw, pw
		err = cmd.Start()
		_ = pw.Close()
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "twr-claude: claude setup-token: %v\n", err)
			return ExitFailed
		}
		token = forward(pr, stderr)
		if err := cmd.Wait(); err != nil {
			_, _ = fmt.Fprintf(stderr, "twr-claude: claude setup-token: %v\n", err)
			return ExitFailed
		}
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

// firstLine returns the first line of r that holds something, trimmed, so Enter
// ends the paste and Ctrl+D is not needed. A last line without a newline
// counts; an empty result means r held nothing but blank lines.
func firstLine(r io.Reader) (string, error) {
	in := bufio.NewReader(r)
	for {
		line, err := in.ReadString('\n')
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return trimmed, nil
		}
		if errors.Is(err, io.EOF) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
	}
}

// forward writes what setup-token prints to w as it arrives, so a sign-in URL
// and a prompt without a trailing newline reach the terminal while the CLI
// waits for the code, and returns the first token it saw. Each token is
// written as <token>; a tail that may still become one waits for the next
// read, so no part of a token split across reads gets out.
func forward(r io.Reader, w io.Writer) string {
	var token string
	var held []byte
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		held = append(held, buf[:n]...)
		safe := len(held)
		if err == nil {
			safe -= tokenTail(held)
		}
		part := held[:safe]
		if token == "" {
			token = string(oauthTokenRE.Find(part))
		}
		_, _ = w.Write(oauthTokenRE.ReplaceAll(part, []byte("<token>")))
		held = append([]byte(nil), held[safe:]...)
		if err != nil {
			return token
		}
	}
}

// tokenTail is the length of the suffix of b that more output could turn into
// a token, or lengthen: a token the CLI has not finished printing, or the
// start of its prefix.
func tokenTail(b []byte) int {
	start := len(b)
	for start > 0 && isTokenByte(b[start-1]) {
		start--
	}
	run := b[start:]
	if i := bytes.Index(run, []byte(tokenPrefix)); i >= 0 {
		return len(run) - i
	}
	for n := min(len(run), len(tokenPrefix)-1); n > 0; n-- {
		if bytes.HasSuffix(run, []byte(tokenPrefix[:n])) {
			return n
		}
	}
	return 0
}

// isTokenByte says whether c can appear in a token.
func isTokenByte(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-'
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

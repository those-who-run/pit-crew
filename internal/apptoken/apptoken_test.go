package apptoken

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
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

// A throwaway key, generated per test run. 2048 bits is GitHub's minimum.
func newKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func writePEM(t *testing.T, name, blockType string, der []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// fakeGitHub is the two endpoints Mint uses, checking the JWT against the App's
// public key as GitHub would.
type fakeGitHub struct {
	t        *testing.T
	public   *rsa.PublicKey
	clientID string
	// lastBody is the access_tokens request's body.
	lastBody map[string]any
	// installationStatus, when set, is returned by the installation lookup.
	installationStatus int
	tokenStatus        int
}

func (f *fakeGitHub) verify(r *http.Request) bool {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		f.t.Errorf("no bearer JWT on %s", r.URL.Path)
		return false
	}
	parts := strings.Split(strings.TrimPrefix(auth, "Bearer "), ".")
	if len(parts) != 3 {
		f.t.Errorf("JWT has %d parts", len(parts))
		return false
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || rsa.VerifyPKCS1v15(f.public, crypto.SHA256, digest[:], signature) != nil {
		f.t.Errorf("JWT signature does not verify")
		return false
	}
	claimsJSON, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims struct {
		Iss string `json:"iss"`
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
	}
	if err := json.Unmarshal(claimsJSON, &claims); err != nil || claims.Iss != f.clientID {
		f.t.Errorf("claims %s", claimsJSON)
		return false
	}
	if claims.Exp-claims.Iat > 600 || claims.Iat > time.Now().Unix() {
		f.t.Errorf("JWT lifetime iat=%d exp=%d", claims.Iat, claims.Exp)
	}
	if r.Header.Get("Accept") != "application/vnd.github+json" || r.Header.Get("X-GitHub-Api-Version") == "" {
		f.t.Errorf("headers %v", r.Header)
	}
	return true
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !f.verify(r) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/orgs/those-who-run/installation":
		if f.installationStatus != 0 {
			w.WriteHeader(f.installationStatus)
			_, _ = fmt.Fprint(w, `{"message": "Not Found"}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"id": 77, "account": {"login": "those-who-run"}}`)
	case r.Method == http.MethodPost && r.URL.Path == "/app/installations/77/access_tokens":
		data, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(data, &f.lastBody); err != nil {
			f.t.Errorf("body %s", data)
		}
		if f.tokenStatus != 0 {
			w.WriteHeader(f.tokenStatus)
			_, _ = fmt.Fprint(w, `{"message": "Resource not accessible by integration"}`)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprint(w, `{"token": "ghs_`+strings.Repeat("x", 36)+`", "expires_at": "2026-11-06T10:00:00Z", "permissions": {"contents": "read"}}`)
	default:
		f.t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func setup(t *testing.T) (*fakeGitHub, *httptest.Server, *rsa.PrivateKey) {
	t.Helper()
	key := newKey(t)
	fake := &fakeGitHub{t: t, public: &key.PublicKey, clientID: "Iv1.0123456789abcdef"}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	return fake, server, key
}

func writeText(t *testing.T, name, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestMintScopesTheToken(t *testing.T) {
	fake, server, key := setup(t)
	for name, path := range map[string]string{
		"PKCS#1": writePEM(t, "pkcs1.pem", "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(key)),
		"PKCS#8": func() string {
			der, err := x509.MarshalPKCS8PrivateKey(key)
			if err != nil {
				t.Fatal(err)
			}
			return writePEM(t, "pkcs8.pem", "PRIVATE KEY", der)
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			cfg := Config{ClientID: fake.clientID, KeyPath: path, APIURL: server.URL + "/"}
			token, err := Mint(t.Context(), server.Client(), cfg, []string{"those-who-run/ecu", "those-who-run/paddock"},
				Permissions{"contents": "read", "pull_requests": "write"})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(token.Value, "ghs_") || !token.ExpiresAt.Equal(time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)) {
				t.Errorf("token %+v", token)
			}
			want := map[string]any{
				"repositories": []any{"ecu", "paddock"},
				"permissions":  map[string]any{"contents": "read", "pull_requests": "write"},
			}
			if fmt.Sprint(fake.lastBody) != fmt.Sprint(want) {
				t.Errorf("asked for %v, want %v", fake.lastBody, want)
			}
		})
	}
}

func TestMintWithAKnownInstallation(t *testing.T) {
	fake, server, key := setup(t)
	fake.installationStatus = http.StatusNotFound // the lookup must not be called
	cfg := Config{ClientID: fake.clientID, KeyPath: writePEM(t, "k.pem", "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(key)),
		InstallationID: 77, APIURL: server.URL}
	if _, err := Mint(t.Context(), server.Client(), cfg, []string{"those-who-run/ecu"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, asked := fake.lastBody["permissions"]; asked {
		t.Errorf("permissions sent when none were asked for: %v", fake.lastBody)
	}
}

func TestMintErrors(t *testing.T) {
	fake, server, key := setup(t)
	good := writePEM(t, "k.pem", "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(key))
	ecdsaLike := writePEM(t, "cert.pem", "CERTIFICATE", []byte("not a key"))
	cases := []struct {
		name  string
		cfg   Config
		repos []string
		setup func()
		want  string
	}{
		{"no repositories", Config{ClientID: fake.clientID, KeyPath: good, APIURL: server.URL}, nil, nil, "no repositories"},
		{"not owner/name", Config{ClientID: fake.clientID, KeyPath: good, APIURL: server.URL}, []string{"ecu"}, nil, "not owner/name"},
		{"two owners", Config{ClientID: fake.clientID, KeyPath: good, APIURL: server.URL}, []string{"a/x", "b/y"}, nil, "different owners"},
		{"no key path", Config{ClientID: fake.clientID, APIURL: server.URL}, []string{"those-who-run/ecu"}, nil, "no key path"},
		{"a missing key", Config{ClientID: fake.clientID, KeyPath: filepath.Join(t.TempDir(), "none.pem"), APIURL: server.URL}, []string{"those-who-run/ecu"}, nil, "read key"},
		{"not PEM", Config{ClientID: fake.clientID, KeyPath: writeText(t, "x.txt", "just text"), APIURL: server.URL}, []string{"those-who-run/ecu"}, nil, "not a PEM file"},
		{"not a key", Config{ClientID: fake.clientID, KeyPath: ecdsaLike, APIURL: server.URL}, []string{"those-who-run/ecu"}, nil, "not a private key"},
		{"a corrupt key", Config{ClientID: fake.clientID, KeyPath: writePEM(t, "bad.pem", "RSA PRIVATE KEY", []byte("junk")), APIURL: server.URL}, []string{"those-who-run/ecu"}, nil, "bad.pem"},
		{"no client ID", Config{KeyPath: good, APIURL: server.URL}, []string{"those-who-run/ecu"}, nil, "no client ID"},
		{"not installed", Config{ClientID: fake.clientID, KeyPath: good, APIURL: server.URL}, []string{"those-who-run/ecu"},
			func() { fake.installationStatus = http.StatusNotFound }, "404"},
		{"a permission the App lacks", Config{ClientID: fake.clientID, KeyPath: good, APIURL: server.URL}, []string{"those-who-run/ecu"},
			func() { fake.tokenStatus = http.StatusUnprocessableEntity }, "not accessible"},
		{"no server", Config{ClientID: fake.clientID, KeyPath: good, APIURL: "http://127.0.0.1:1"}, []string{"those-who-run/ecu"}, nil, "connect"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake.installationStatus, fake.tokenStatus = 0, 0
			if c.setup != nil {
				c.setup()
			}
			_, err := Mint(t.Context(), server.Client(), c.cfg, c.repos, nil)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %v, want one containing %q", err, c.want)
			}
		})
	}
}

func TestTheWrongKeyIsNotVerified(t *testing.T) {
	// The fake reports a 401 for a bad signature through t.Errorf; this test
	// checks the path without failing the suite.
	fake, server, _ := setup(t)
	other := newKey(t)
	quiet := &fakeGitHub{t: &testing.T{}, public: fake.public, clientID: fake.clientID}
	server.Config.Handler = quiet
	cfg := Config{ClientID: fake.clientID, KeyPath: writePEM(t, "other.pem", "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(other)), APIURL: server.URL}
	_, err := Mint(t.Context(), server.Client(), cfg, []string{"those-who-run/ecu"}, nil)
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("error %v, want 401", err)
	}
}

func TestATokenPrintsNothing(t *testing.T) {
	token := Token{Value: "ghs_secret"}
	if s := fmt.Sprintf("%v %s %+v", token, token, token); strings.Contains(s, "secret") {
		t.Errorf("printed %q", s)
	}
}

func TestInstallationWithNoID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{}`)
	}))
	defer server.Close()
	key := newKey(t)
	cfg := Config{ClientID: "x", KeyPath: writePEM(t, "k.pem", "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(key)), APIURL: server.URL}
	if _, err := Mint(t.Context(), server.Client(), cfg, []string{"those-who-run/ecu"}, nil); err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Errorf("error %v", err)
	}
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"id": 1, "token": ""}`)
	})
	if _, err := Mint(t.Context(), server.Client(), cfg, []string{"those-who-run/ecu"}, nil); err == nil || !strings.Contains(err.Error(), "no token") {
		t.Errorf("error %v", err)
	}
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `not json`)
	})
	if _, err := Mint(t.Context(), server.Client(), cfg, []string{"those-who-run/ecu"}, nil); err == nil || !strings.Contains(err.Error(), "invalid character") {
		t.Errorf("error %v", err)
	}
}

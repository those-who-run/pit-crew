package privatescan

// The address, host, context and allowlist cases are those of
// tests/test_private_scan.py in those-who-run/paddock, ported one for one. Every
// value that must fail is built from its parts and never appears whole, as there:
// this repository is public, and its own checks read this file.

import (
	"reflect"
	"strings"
	"testing"
)

var privateIP = "10.20.30." + "40"

func dot(labels ...string) string { return strings.Join(labels, ".") }

var privateHost = dot("nuc", "home", "arpa")

const (
	ctx       = "context"
	testAllow = "github.com  The org's code host.\nk8s.io  Kubernetes API groups.\n"
)

func scanner(t *testing.T) Scanner {
	t.Helper()
	allow, err := (&Allowlist{}).Add(testAllow, "test.allow")
	if err != nil {
		t.Fatal(err)
	}
	return Scanner{Allow: allow}
}

func scan(t *testing.T, text string) []Finding {
	t.Helper()
	return scanner(t).Scan(text)
}

func rules(findings []Finding) []string {
	var out []string
	for _, f := range findings {
		out = append(out, f.Rule)
	}
	return out
}

func wantOne(t *testing.T, text, rule, value string) {
	t.Helper()
	got := scan(t, text)
	if len(got) != 1 || got[0].Rule != rule || (value != "" && got[0].Value != value) {
		t.Errorf("%q: findings %+v, want one %s %s", text, got, rule, value)
	}
}

func wantNone(t *testing.T, text string) {
	t.Helper()
	if got := scan(t, text); len(got) != 0 {
		t.Errorf("%q: findings %+v, want none", text, got)
	}
}

func TestAddresses(t *testing.T) {
	t.Run("a private address fails with its rule and line", func(t *testing.T) {
		got := scan(t, "one\nthe machine is at "+privateIP+":4222\n")
		want := []Finding{{Rule: "address", Line: 2, Value: privateIP}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %+v, want %+v", got, want)
		}
	})
	t.Run("any address outside the ranges fails", func(t *testing.T) {
		for _, ip := range []string{"192.168.1." + "2", "172.16.0." + "9", "8.8.8." + "8", "192.0.3." + "1"} {
			wantOne(t, "addr "+ip+"\n", "address", ip)
		}
	})
	t.Run("an address after an escape fails", func(t *testing.T) {
		wantOne(t, `"a\n`+privateIP+`"`+"\n", "address", privateIP)
	})
	t.Run("documentation, loopback and unspecified pass", func(t *testing.T) {
		wantNone(t, "192.0.2.10 198.51.100.7/24 203.0.113.255 127.0.0.1 127.1.2.3 0.0.0.0:4222\n")
	})
	t.Run("versions and numbers that are not addresses pass", func(t *testing.T) {
		wantNone(t, "rev v0.11.0.1, 1.2.3.4.5, 300.1.2.3, oid 1.3.6.1.4.1\n")
	})
	t.Run("an address at the end of a sentence fails", func(t *testing.T) {
		wantOne(t, "It listens on "+privateIP+".\n", "address", privateIP)
	})
	t.Run("two addresses on a line both fail", func(t *testing.T) {
		if got := scan(t, privateIP+" and 192.168.1."+"2\n"); len(got) != 2 {
			t.Errorf("findings %+v, want two", got)
		}
	})
}

func TestHosts(t *testing.T) {
	t.Run("a bare host fails", func(t *testing.T) {
		wantOne(t, "ssh "+privateHost+"\n", "host", privateHost)
	})
	t.Run("a URL host fails with user and port", func(t *testing.T) {
		wantOne(t, "url: nats://twr:secret@"+privateHost+":4222\n", "host", privateHost)
	})
	t.Run("a URL host with any top-level domain fails", func(t *testing.T) {
		wantOne(t, "see https://"+dot("pitwall", "my-lab", "sh")+"/x\n", "host", "")
	})
	t.Run("a host after user@ fails whatever its top-level domain", func(t *testing.T) {
		wantOne(t, "ssh admin@"+dot("nuc", "xyzzy")+"\n", "host", dot("nuc", "xyzzy"))
		wantOne(t, "git clone git@"+dot("gitea", "sh")+":me/x\n", "host", dot("gitea", "sh"))
	})
	t.Run("at signs that are not hosts pass", func(t *testing.T) {
		wantNone(t, "img@sha256:abc tool@v1.2.3 x@users.noreply.github.com @contextlib.contextmanager me@example.com\n")
	})
	t.Run("an e-mail domain fails", func(t *testing.T) {
		wantOne(t, "mail me@"+"someone."+"ie\n", "host", "someone."+"ie")
	})
	t.Run("placeholders pass", func(t *testing.T) {
		wantNone(t, "host: nats.example.com\nhttps://example.com/x a.example b.example.org c.test d.invalid\n"+
			"nats://nats.twr.svc.cluster.local:4222 logbook.twr.svc.cluster.local http://localhost:8080\n")
	})
	t.Run("mDNS .local is not a placeholder", func(t *testing.T) {
		wantOne(t, dot("printer", "local")+"\n", "host", "")
		wantOne(t, "http://"+dot("nuc", "local")+":8080\n", "host", "")
	})
	t.Run("wildcard and leading-dot hosts fail", func(t *testing.T) {
		for _, line := range []string{
			"*." + dot("home", "lan"),
			"NO_PROXY=." + dot("home", "lan"),
			"domain ." + dot("pitwall", "home", "lan"),
			`["*.` + dot("nuc", "lab") + `"]`,
			"." + dot("home", "lan"),
		} {
			wantOne(t, line+"\n", "host", "")
		}
	})
	t.Run("home network suffixes fail", func(t *testing.T) {
		for _, tld := range []string{"box", "lab", "homelab", "private", "lan", "internal"} {
			wantOne(t, "ssh "+dot("nuc", tld)+"\n", "host", dot("nuc", tld))
		}
	})
	t.Run("code ending like a top-level domain passes", func(t *testing.T) {
		wantNone(t, "metadata.labels.app spec.site page.info self.me Path.home() x.build\n")
	})
	t.Run("names with no dot pass", func(t *testing.T) {
		wantNone(t, "TWR_NATS_URL=nats://nats:4222 http://logbook:8080 nats://user:pass@host:4222\n")
	})
	t.Run("the allowlist allows a host and the names under it", func(t *testing.T) {
		wantNone(t, "https://github.com/those-who-run x@users.noreply.github.com networking.k8s.io/v1\n")
	})
	t.Run("a name that only ends like an allowed host fails", func(t *testing.T) {
		wantOne(t, "https://"+dot("notgithub", "com")+"/x\n", "host", dot("notgithub", "com"))
	})
	t.Run("file names, code and subjects pass", func(t *testing.T) {
		wantNone(t, "compose.yaml bootstrap.sh deploy_check.py README.md sys.argv twr.v1.*.frame.gt7.lo\n")
	})
	t.Run("an escape is a separator", func(t *testing.T) {
		wantNone(t, `"a\nk8s.io\tgithub.com"`+"\n")
		wantOne(t, `"a\n`+privateHost+`"`+"\n", "host", privateHost)
	})
	t.Run("templates pass", func(t *testing.T) {
		wantNone(t, "https://${HOST}/x nats://$host:4222\n")
	})
	t.Run("a host at the end of a sentence fails once, without the full stop", func(t *testing.T) {
		wantOne(t, "It is served from "+privateHost+".\n", "host", privateHost)
	})
	t.Run("the same host twice on a line is one finding", func(t *testing.T) {
		wantOne(t, privateHost+" http://"+privateHost+"/\n", "host", privateHost)
	})
	t.Run("a call is not a host", func(t *testing.T) {
		wantNone(t, "os.home() cfg.local(x)\n")
	})
	t.Run("with no allowlist a public host fails too", func(t *testing.T) {
		if got := (Scanner{}).Scan("https://github.com/x\n"); len(got) != 1 {
			t.Errorf("findings %+v, want one", got)
		}
	})
}

func TestContexts(t *testing.T) {
	t.Run("a cluster context fails", func(t *testing.T) {
		for _, line := range []string{
			"kubectl --" + ctx + " nuc get pods",
			"kubectl --" + ctx + "=nuc get pods",
			"kubectl config use-" + ctx + " nuc",
			"current-" + ctx + ": nuc",
		} {
			wantOne(t, line+"\n", "context", "nuc")
		}
		wantOne(t, "nats --"+ctx+` "nuc-admin" sub x`+"\n", "context", "nuc-admin")
	})
	t.Run("placeholders pass", func(t *testing.T) {
		for _, value := range []string{"my-cluster", "<context>", `"$CTX"`, "${CONTEXT}", "{{ .context }}", "example"} {
			wantNone(t, "kubectl --"+ctx+" "+value+" get pods\n")
		}
		wantNone(t, "nats --no-"+ctx+" sub x\nflags = [\"--"+ctx+"\", \"-H\"]\n")
	})
}

func TestWordBoundariesKnowEveryScript(t *testing.T) {
	// The reference's \b treats é as a letter; Go's does not. These pass there.
	wantNone(t, "caféuse-"+ctx+" nuc\n")
	wantNone(t, "kubectl --"+ctx+" my-clüster get pods\n")
	// The scheme may still start later in the same word.
	wantOne(t, "éa-b://"+privateHost+"/x\n", "host", privateHost)
}

func TestCodeMode(t *testing.T) {
	code := scanner(t)
	code.Code = true
	if got := code.Scan("loc := time.Local // cfg.Internal, opts.Private\n"); len(got) != 0 {
		t.Errorf("selectors found as %+v", got)
	}
	if got := scan(t, "loc := time.Local\n"); len(got) != 1 {
		t.Errorf("as prose, findings %+v, want the dotted name", got)
	}
	got := code.Scan("const url = \"http://" + privateHost + ":8080\" // or admin@" + dot("nuc", "lan") + ", " + privateIP + "\n")
	if !reflect.DeepEqual(rules(got), []string{"address", "host", "host"}) {
		t.Errorf("in code, findings %+v", got)
	}
}

func TestHomePaths(t *testing.T) {
	wantOne(t, "I ran it in /home/"+"jane/src/ecu\n", "home-path", "/home/"+"jane")
	wantOne(t, "open /Users/"+"jane.doe/notes.md\n", "home-path", "/Users/"+"jane.doe")
	wantNone(t, "HOME=/home/nonroot, /home/runner/work, /home/user/.cache, /home/<user>/x, ~/src, /homestead/x\n")
}

func TestSecrets(t *testing.T) {
	for _, value := range []string{
		"ghp_" + strings.Repeat("a1B2", 9),
		"ghs_" + strings.Repeat("a1B2", 9),
		"github_pat_" + strings.Repeat("a1_B", 12),
		"sk-ant-" + "oat01-" + strings.Repeat("aB3-", 10),
		"xoxb-" + "1234567890-abcdef",
		"-----BEGIN RSA " + "PRIVATE KEY-----",
		"-----BEGIN " + "PRIVATE KEY-----",
	} {
		wantOne(t, "token: "+value+"\n", "secret", value)
	}
	wantNone(t, "the ghp_ prefix, sk-ant- keys, -----BEGIN PUBLIC KEY-----, ghp_short\n")
}

func TestExtraPatterns(t *testing.T) {
	extras, err := ParseExtra("# the lab's own names\n\nlab-host \\bgarage-[a-z]+\\b\nlab-word  paddock club  \n", "extra.txt")
	if err != nil {
		t.Fatal(err)
	}
	s := scanner(t)
	s.Extra = extras
	got := s.Scan("fine\nssh GARAGE-pi then the Paddock Club\n")
	want := []Finding{{Rule: "lab-host", Line: 2, Value: "GARAGE-pi"}, {Rule: "lab-word", Line: 2, Value: "Paddock Club"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestBadExtraPatternsAreRefused(t *testing.T) {
	cases := map[string]string{
		"no pattern":                      "lab-host\n",
		"only spaces after":               "lab-host   \n",
		"a name with a capital":           "Lab secret\n",
		"a pattern that does not compile": "lab-host garage-[a-z\n",
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseExtra("# first\n"+text, "extra.txt")
			if err == nil || !strings.Contains(err.Error(), "extra.txt:2") {
				t.Fatalf("error %v, want one naming extra.txt:2", err)
			}
			// The pattern is the lab's: an error must not repeat it.
			if strings.Contains(err.Error(), "garage") || strings.Contains(err.Error(), "secret") {
				t.Errorf("error %q repeats the pattern", err)
			}
		})
	}
}

func TestAllowlist(t *testing.T) {
	bad := map[string]struct{ text, want string }{
		"an entry without a reason": {"# c\n\ngithub.com\n", "a.allow:3: github.com has no reason"},
		"a home network's name":     {dot("nuc", "lan") + " why\n", "is a home network's name, never a public host"},
		"mDNS":                      {dot("printer", "local") + " why\n", "is a home network's name"},
		"a home.arpa name":          {dot("x", "home", "arpa") + " why\n", "is a home network's name"},
		"not a host":                {"localhost why\n", "localhost is not a host name"},
	}
	for name, c := range bad {
		t.Run(name, func(t *testing.T) {
			_, err := (&Allowlist{}).Add(c.text, "a.allow")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %v, want one containing %q", err, c.want)
			}
		})
	}
	t.Run("adding keeps the entries already there", func(t *testing.T) {
		allow, err := DefaultAllowlist().Add(dot("status", "example-vendor", "io")+" A vendor's status page.\n", "job.allow")
		if err != nil {
			t.Fatal(err)
		}
		s := Scanner{Allow: allow}
		if got := s.Scan("https://github.com/x https://" + dot("status", "example-vendor", "io") + "/y\n"); len(got) != 0 {
			t.Errorf("findings %+v, want none", got)
		}
	})
}

func TestTheDefaultAllowlistIsWellFormed(t *testing.T) {
	allow := DefaultAllowlist()
	if len(allow.hosts) < 10 {
		t.Fatalf("only %d hosts", len(allow.hosts))
	}
	// What a review comment and a pull request body always carry must pass.
	text := "Closes those-who-run/paddock#232.\n" +
		"🤖 Generated with [Claude Code](https://claude.com/claude-code)\n" +
		"Co-Authored-By: Claude <noreply@anthropic.com>\n" +
		"See https://github.com/those-who-run/ecu/pull/31 and pkg.go.dev/net/http.\n"
	if got := (Scanner{Allow: allow}).Scan(text); len(got) != 0 {
		t.Errorf("findings %+v, want none", got)
	}
}

func TestFindingStringHidesTheValue(t *testing.T) {
	f := Finding{Rule: "host", Line: 3, Value: privateHost}
	if got := f.String(); got != "host: line 3" || strings.Contains(got, privateHost) {
		t.Errorf("String() = %q", got)
	}
}

func TestSplitLines(t *testing.T) {
	got := splitLines("a\nb\r\nc\rd e\n")
	want := []string{"a", "b", "c", "d", "e"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := splitLines(""); len(got) != 0 {
		t.Errorf("empty text split as %q", got)
	}
	if got := rules(Scanner{}.Scan("x\r\n" + privateIP)); !reflect.DeepEqual(got, []string{"address"}) {
		t.Errorf("rules %v", got)
	}
}

func TestARealisticComment(t *testing.T) {
	comment := "Review at head `1f0c5d2e`: **changes asked for**.\n\n" +
		"**Criteria:**\n- AC1 fuel callout: proven, by `race/fuel_test.go`, `TestBoxThisLap`, on a recording.\n\n" +
		"**Checked:** I ran `make check` (green, 41.2 s) and `go test ./race/ -run TestBoxThisLap -count=20`.\n\n" +
		"**Asked for:**\n1. `race/fuel.go:88-97 (head 1f0c5d2e)`: the margin is compared in litres against laps. " +
		"It fails when `lapsLeft` is 0.5: the callout fires a lap late. Please convert before comparing.\n\n" +
		"**Not run here:** the NATS integration test, which needs a server at nats://nats:4222.\n\n" +
		"<!-- twr-review sha=1f0c5d2e9a7b4c3d8e6f1a2b3c4d5e6f7a8b9c0d kind=A lane=standard -->\n"
	if got := (Scanner{Allow: DefaultAllowlist()}).Scan(comment); len(got) != 0 {
		t.Errorf("a clean comment has findings %+v", got)
	}
	planted := strings.Replace(comment, "nats://nats:4222", "nats://"+privateHost+":4222 ("+privateIP+")", 1)
	got := (Scanner{Allow: DefaultAllowlist()}).Scan(planted)
	if !reflect.DeepEqual(rules(got), []string{"address", "host"}) || got[0].Line != 11 {
		t.Errorf("planted values found as %+v", got)
	}
}

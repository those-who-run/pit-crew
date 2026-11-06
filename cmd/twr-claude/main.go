// Command twr-claude runs Claude Code on the maintainer's subscription: `run` is
// the agent step's entrypoint, `login` and `status` are the maintainer's, on his
// own machine. docs/twr-claude.md says how, and what was checked against the CLI.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/those-who-run/pit-crew/internal/claude"
)

var version = "dev"

const usage = `usage: twr-claude [-version] <command>

commands:
  run                   start claude -p for one task with the mounted token (TWR_CLAUDE_*)
  login [--from-stdin]  make a setup-token token and write it into the cluster's Secret
  status                say how old the Secret's token is, never its value
`

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Getenv, os.Environ(), os.Stdin, os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, getenv func(string) string, environ []string, stdin io.Reader, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	flags := flag.NewFlagSet("twr-claude", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { _, _ = fmt.Fprint(stderr, usage) }
	showVersion := flags.Bool("version", false, "print the version and exit")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *showVersion {
		_, _ = fmt.Fprintln(stdout, version)
		return 0
	}
	if flags.NArg() < 1 {
		flags.Usage()
		return 2
	}
	rest := flags.Args()[1:]
	switch command := flags.Arg(0); command {
	case "run":
		cfg, err := claude.RunConfigFromEnv(getenv)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "twr-claude run: %v\n", err)
			return claude.ExitConfig
		}
		return claude.Run(ctx, cfg, environ, stderr)
	case "login", "status":
		cfg, err := claude.SecretConfigFromEnv(getenv)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "twr-claude %s: %v\n", command, err)
			return claude.ExitConfig
		}
		if command == "status" {
			return claude.Status(ctx, cfg, stdout, stderr)
		}
		sub := flag.NewFlagSet("twr-claude login", flag.ContinueOnError)
		sub.SetOutput(stderr)
		fromStdin := sub.Bool("from-stdin", false, "read the token from standard input instead of running claude setup-token")
		if err := sub.Parse(rest); err != nil {
			return 2
		}
		return claude.Login(ctx, cfg, *fromStdin, stdin, stderr)
	default:
		_, _ = fmt.Fprintf(stderr, "twr-claude: unknown command %q\n%s", command, usage)
		return 2
	}
}

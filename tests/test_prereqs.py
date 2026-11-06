"""scripts/prereqs.sh, the Makefile's `prereqs`, which `make check` and `make hooks`
run first: one line for each tool that is missing or can't be used, with the fix,
and `make hooks` and `make check` stop on it before their first step.

Each case runs it in a temporary copy of the Makefile, the script and a go.mod,
with PATH holding only fake tools: a `go` that answers `go env` with the version
and settings the case gives, and stubs for the rest. `make` is the real one, and
`pre-commit` writes a file when it runs, so a case can tell whether a target got
that far. Needs make; standard library only."""

from __future__ import annotations

import os
import shutil
import subprocess
import tempfile
import time
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
MAKE = shutil.which("make")
GO_MOD = "module example.com/fixture\n\ngo 1.27.0\n\ntoolchain go1.27.3\n"
WANT = "go1.27.3"
# The tools the check looks for, beside go and make.
STUBS = ("gofmt", "gcc", "python3", "pre-commit", "git")


class Prereqs(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.tmp = Path(tmp.name)
        self.module = self.tmp / "module"
        (self.module / "scripts").mkdir(parents=True)
        shutil.copy2(ROOT / "Makefile", self.module / "Makefile")
        shutil.copy2(ROOT / "scripts" / "prereqs.sh", self.module / "scripts" / "prereqs.sh")
        (self.module / "go.mod").write_text(GO_MOD, encoding="utf-8")
        self.bin = self.tmp / "bin"
        self.bin.mkdir()
        self.goroot = self.tmp / "goroot"
        self.goroot.mkdir()
        self.goenv = self.tmp / "goenv"
        self.ran = self.tmp / "pre-commit-ran"
        self.go(WANT)
        for name in STUBS:
            self.stub(name)
        self.stub("pre-commit", f'echo "$@" >> "{self.ran}"')
        self.assertIsNotNone(MAKE, "make is needed")
        os.symlink(MAKE, self.bin / "make")

    def stub(self, name: str, body: str = "") -> None:
        path = self.bin / name
        path.write_text(f"#!/bin/sh\n{body}\n", encoding="utf-8")
        path.chmod(0o755)

    def go(self, version: str, warning: str = "") -> None:
        """A go whose own version is `version`: `go env` prints what the script asks
        for, GOVERSION GOENV GOROOT CC, after `warning` on standard error as the
        real one may. Anything else fails, so the check runs no other go command,
        and so does a run outside / or without GOTOOLCHAIN=local, where the real one
        would read go.mod and could switch toolchains, which needs the network."""
        warn = f'echo "{warning}" >&2\n' if warning else ""
        self.stub(
            "go",
            'if [ "$GOTOOLCHAIN" != local ] || [ "$(pwd)" != / ]; then\n'
            '  echo "go run in $(pwd) with GOTOOLCHAIN=$GOTOOLCHAIN" >&2; exit 2\nfi\n'
            f"{warn}"
            'case "$*" in\n'
            f'"env GOVERSION GOENV GOROOT CC") printf "%s\\n" {version} {self.goenv} {self.goroot} gcc ;;\n'
            '*) echo "unexpected: go $*" >&2; exit 2 ;;\nesac',
        )

    def remove(self, name: str) -> None:
        (self.bin / name).unlink()

    def run_make(self, target: str, **env: str) -> subprocess.CompletedProcess[str]:
        environ = {"PATH": str(self.bin), "HOME": str(self.tmp), **env}
        return subprocess.run(
            [MAKE, "--no-print-directory", target],
            cwd=self.module, env=environ, capture_output=True, text=True, timeout=30,
        )

    def errors(self, done: subprocess.CompletedProcess[str]) -> list[str]:
        """The check's lines, without make's own."""
        return [l for l in done.stderr.splitlines() if not l.startswith("make")]

    def test_every_tool_there_passes_quickly(self):
        start = time.monotonic()
        done = self.run_make("prereqs", GOTOOLCHAIN="local")
        self.assertLess(time.monotonic() - start, 2)
        self.assertEqual(done.returncode, 0, done.stderr)
        self.assertEqual(done.stderr, "")
        self.assertIn("prereqs: every tool", done.stdout)

    def test_with_every_tool_make_hooks_installs_them(self):
        done = self.run_make("hooks")
        self.assertEqual(done.returncode, 0, done.stderr)
        self.assertEqual(self.ran.read_text(encoding="utf-8"), "install\n")

    def test_pre_commit_missing_names_how_to_install_it(self):
        self.remove("pre-commit")
        done = self.run_make("prereqs")
        self.assertNotEqual(done.returncode, 0)
        [line] = self.errors(done)
        self.assertTrue(line.startswith("pre-commit: not found;"), line)
        self.assertIn("pipx install pre-commit", line)

    def test_make_hooks_stops_before_pre_commit_install(self):
        # pre-commit is there but python3 isn't: the check fails, so pre-commit
        # never runs.
        self.remove("python3")
        done = self.run_make("hooks")
        self.assertNotEqual(done.returncode, 0)
        self.assertFalse(self.ran.exists(), "make hooks ran pre-commit")
        self.assertIn("python3: not found;", done.stderr)

    def test_make_check_stops_before_its_first_step(self):
        self.go("go1.26.9")
        done = self.run_make("check", GOTOOLCHAIN="local")
        self.assertNotEqual(done.returncode, 0)
        self.assertFalse(self.ran.exists(), "make check ran pre-commit")
        self.assertIn("GOTOOLCHAIN=local", done.stderr)

    def test_one_line_for_each_missing_tool(self):
        for name in ("python3", "pre-commit", "git", "gcc", "gofmt"):
            self.remove(name)
        done = self.run_make("prereqs")
        self.assertNotEqual(done.returncode, 0)
        self.assertEqual(
            [l.split(":")[0] for l in self.errors(done)],
            ["gofmt", "gcc", "python3", "pre-commit", "git"],
        )

    def test_go_missing_names_the_version_to_install(self):
        self.remove("go")
        [line] = self.errors(self.run_make("prereqs"))
        self.assertTrue(line.startswith("go: not found;"), line)
        self.assertIn(f"install {WANT} from https://go.dev/dl/", line)

    def test_an_old_go_with_gotoolchain_local_names_gotoolchain(self):
        self.go("go1.26.9")
        done = self.run_make("prereqs", GOTOOLCHAIN="local")
        self.assertNotEqual(done.returncode, 0)
        [line] = self.errors(done)
        self.assertIn(f"go1.26.9 is older than {WANT}", line)
        self.assertIn("GOTOOLCHAIN=local (set in the environment)", line)
        self.assertIn("unset GOTOOLCHAIN or set it to auto", line)
        self.assertIn(f"install {WANT}", line)

    def test_unsetting_gotoolchain_isnt_offered_where_go_env_says_local(self):
        self.go("go1.26.9")
        (self.goroot / "go.env").write_text("GOTOOLCHAIN=local\n", encoding="utf-8")
        [line] = self.errors(self.run_make("prereqs", GOTOOLCHAIN="local"))
        self.assertIn("GOTOOLCHAIN=local (set in the environment)", line)
        self.assertIn(
            f"set GOTOOLCHAIN to auto (unsetting it leaves GOTOOLCHAIN=local, set in {self.goroot}/go.env)",
            line,
        )
        self.assertNotIn("unset GOTOOLCHAIN or", line)

    def test_a_go_mod_without_a_version(self):
        (self.module / "go.mod").write_text("module example.com/fixture\n", encoding="utf-8")
        self.assertEqual(self.run_make("prereqs", GOTOOLCHAIN="local").returncode, 0)
        self.remove("go")
        [line] = self.errors(self.run_make("prereqs"))
        self.assertEqual(
            line,
            "go: not found; install Go from https://go.dev/dl/ (go.mod names no version)"
            " and put its bin directory on PATH",
        )

    def test_gotoolchain_local_from_the_go_installation(self):
        # As Fedora's Go sets it, in $GOROOT/go.env; unsetting nothing helps there.
        self.go("go1.26.10-X:nodwarf5")
        (self.goroot / "go.env").write_text("GOPROXY=direct\nGOTOOLCHAIN=local\n", encoding="utf-8")
        [line] = self.errors(self.run_make("prereqs"))
        self.assertIn(f"go1.26.10-X:nodwarf5 is older than {WANT}", line)
        self.assertIn(f"GOTOOLCHAIN=local (set in {self.goroot}/go.env)", line)
        self.assertIn("`go env -w GOTOOLCHAIN=auto`", line)

    def test_an_empty_gotoolchain_counts_as_unset(self):
        # As for the go command: go.env's local applies, not an empty value.
        self.go("go1.26.9")
        (self.goroot / "go.env").write_text("GOTOOLCHAIN=local\n", encoding="utf-8")
        [line] = self.errors(self.run_make("prereqs", GOTOOLCHAIN=""))
        self.assertIn(f"GOTOOLCHAIN=local (set in {self.goroot}/go.env)", line)

    def test_a_warning_from_go_doesnt_shift_its_answers(self):
        # The real go warns on standard error when GOPATH is GOROOT, say.
        warning = "warning: both GOPATH and GOROOT are the same directory"
        self.go("go1.26.9", warning=warning)
        (self.goroot / "go.env").write_text("GOTOOLCHAIN=local\n", encoding="utf-8")
        [line] = self.errors(self.run_make("prereqs"))
        self.assertIn(f"GOTOOLCHAIN=local (set in {self.goroot}/go.env)", line)
        self.go(WANT, warning=warning)
        done = self.run_make("prereqs")
        self.assertEqual(done.returncode, 0, done.stderr)

    def test_a_go_env_that_fails_says_why(self):
        self.stub("go", 'echo "go: cannot find GOROOT directory" >&2; exit 1')
        [line] = self.errors(self.run_make("prereqs"))
        self.assertIn("go: `go env` failed (go: cannot find GOROOT directory);", line)

    def test_go_env_w_wins_over_the_go_installation(self):
        self.go("go1.26.9")
        (self.goroot / "go.env").write_text("GOTOOLCHAIN=local\n", encoding="utf-8")
        self.goenv.write_text("GOTOOLCHAIN=auto\n", encoding="utf-8")
        self.assertEqual(self.run_make("prereqs").returncode, 0)
        self.goenv.write_text("GOTOOLCHAIN=local\n", encoding="utf-8")
        [line] = self.errors(self.run_make("prereqs"))
        self.assertIn(f"GOTOOLCHAIN=local (set in {self.goenv})", line)

    def test_an_old_go_that_may_fetch_the_toolchain_passes(self):
        self.go("go1.26.9")
        for toolchain in ("auto", "go1.27.0+auto"):
            with self.subTest(GOTOOLCHAIN=toolchain):
                done = self.run_make("prereqs", GOTOOLCHAIN=toolchain)
                self.assertEqual(done.returncode, 0, done.stderr)
        # Unset everywhere, the go command's default is auto.
        self.assertEqual(self.run_make("prereqs").returncode, 0)

    def test_gotoolchain_path_needs_the_version_on_path(self):
        self.go("go1.26.9")
        [line] = self.errors(self.run_make("prereqs", GOTOOLCHAIN="path"))
        self.assertIn(f"looks on PATH for {WANT}", line)
        self.stub(WANT)
        self.assertEqual(self.run_make("prereqs", GOTOOLCHAIN="path").returncode, 0)

    def test_an_older_pinned_gotoolchain_fails_even_with_a_new_go(self):
        [line] = self.errors(self.run_make("prereqs", GOTOOLCHAIN="go1.27.1"))
        self.assertIn(f"GOTOOLCHAIN=go1.27.1 (set in the environment) is older than {WANT}", line)
        self.assertEqual(self.run_make("prereqs", GOTOOLCHAIN="go1.28.0").returncode, 0)

    def test_a_release_candidate_is_older_than_its_release(self):
        self.go("go1.27rc1")
        done = self.run_make("prereqs", GOTOOLCHAIN="local")
        self.assertIn("go1.27rc1 is older than", done.stderr)
        self.go("go1.27.10")
        self.assertEqual(self.run_make("prereqs", GOTOOLCHAIN="local").returncode, 0)


if __name__ == "__main__":
    unittest.main()

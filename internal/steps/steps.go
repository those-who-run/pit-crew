// Package steps holds what the publish steps share: the scanner built from the
// defaults and the job's own files, the task the fetch step recorded, and the
// hold folder where what cannot be posted goes for the maintainer.
package steps

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/those-who-run/pit-crew/internal/fetch"
	"github.com/those-who-run/pit-crew/internal/privatescan"
)

// The exit codes the publish steps share.
const (
	ExitOK        = 0
	ExitFailed    = 1
	ExitConfig    = 2
	ExitHeld      = 10
	ExitHeadMoved = 11
)

// Scanner builds the private-value scanner: the default allowlist, the job's
// allow file and its patterns file, either of which may be absent.
func Scanner(allowFile, patternsFile string) (privatescan.Scanner, error) {
	allow := privatescan.DefaultAllowlist()
	if allowFile != "" {
		text, err := os.ReadFile(allowFile) //nolint:gosec // the operator's configuration
		if err != nil {
			return privatescan.Scanner{}, fmt.Errorf("allow file: %w", err)
		}
		if allow, err = allow.Add(string(text), allowFile); err != nil {
			return privatescan.Scanner{}, err
		}
	}
	scanner := privatescan.Scanner{Allow: allow}
	if patternsFile != "" {
		text, err := os.ReadFile(patternsFile) //nolint:gosec // the operator's configuration
		if err != nil {
			return privatescan.Scanner{}, fmt.Errorf("patterns file: %w", err)
		}
		if scanner.Extra, err = privatescan.ParseExtra(string(text), patternsFile); err != nil {
			return privatescan.Scanner{}, err
		}
	}
	return scanner, nil
}

// ReadTask reads task.json from the data volume, which the model could not
// write.
func ReadTask(dir string) (fetch.Task, error) {
	var task fetch.Task
	data, err := os.ReadFile(filepath.Join(dir, fetch.DataDir, "task.json")) //nolint:gosec // the data volume
	if err != nil {
		return task, fmt.Errorf("task.json: %w", err)
	}
	if err := json.Unmarshal(data, &task); err != nil {
		return task, fmt.Errorf("task.json: %w", err)
	}
	return task, nil
}

// ReadOutput reads one of the model's output files; nil when it is not there.
func ReadOutput(dir, name string) ([]byte, error) {
	data, err := os.ReadFile(filepath.Join(dir, fetch.OutDir, name)) //nolint:gosec // the output volume, by a fixed name
	if os.IsNotExist(err) {
		return nil, nil
	}
	return data, err
}

// ReadOutputs reads every regular file in the model's output directory, by
// name: what a hold keeps, so the maintainer sees all the model wrote.
// Directories and anything else are left alone.
func ReadOutputs(dir string) (map[string][]byte, error) {
	out := filepath.Join(dir, fetch.OutDir)
	entries, err := os.ReadDir(out)
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(out, e.Name())) //nolint:gosec // the output volume, a name the directory listing gave
		if err != nil {
			return nil, err
		}
		files[e.Name()] = data
	}
	return files, nil
}

var unsafeRE = regexp.MustCompile(`[^a-z0-9-]+`)

// HoldDir makes the folder for one held unit under root and returns it.
func HoldDir(root, repo string, number int, sha string) (string, error) {
	if root == "" {
		return "", fmt.Errorf("no hold directory configured")
	}
	_, name, _ := strings.Cut(repo, "/")
	short := sha
	if len(short) > 8 {
		short = short[:8]
	}
	dir := filepath.Join(root, fmt.Sprintf("%s-%d-%s", unsafeRE.ReplaceAllString(strings.ToLower(name), ""), number, short))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	return dir, nil
}

// Hold writes the files given under dir, skipping nil ones.
func Hold(dir string, files map[string][]byte) error {
	for name, data := range files {
		if data == nil {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			return err
		}
	}
	return nil
}

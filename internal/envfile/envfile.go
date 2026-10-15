// Package envfile loads the repository's own .env into this process's environment at start-up.
// The file is read and never sourced, so a malformed line cannot print fragments of a value
// through a shell's error message, and a name already exported in the environment wins over the
// file. No value is ever printed or logged.
package envfile

import (
	"bufio"
	"os"
	"strings"
)

// unquote removes one pair of matching single or double quotes around value. A quote that is part
// of the value, or that has no match, stays.
func unquote(value string) string {
	if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
		return value[1 : len(value)-1]
	}
	return value
}

// Load reads path, a dotenv-shaped file, and sets each KEY=VALUE it finds into this process's
// environment, skipping blank lines, comments, and any name already set. A missing file is not
// an error: a machine with no .env simply has whatever credentials its own environment holds.
func Load(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		value = unquote(strings.TrimSpace(value))
		if key == "" {
			continue
		}
		if _, alreadySet := os.LookupEnv(key); alreadySet {
			continue
		}
		os.Setenv(key, value)
	}
	return scanner.Err()
}

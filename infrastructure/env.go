package infrastructure

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// loadEnvFile reads a KEY=VALUE file into a map. Blank lines and lines starting
// with '#' are skipped; a line with no '=' is skipped rather than erroring, since
// a malformed line here is far more likely to be a stray comment/typo than a
// value worth failing startup over. The value is split off on the first '=' only,
// so a bot token or password containing '=' survives intact, and a single layer
// of surrounding double or single quotes is stripped. A missing file is not an
// error: it just means every key must come from the real process environment.
func loadEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open env file %q: %w", path, err)
	}
	defer f.Close()

	values := map[string]string{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}

		values[strings.TrimSpace(key)] = unquote(strings.TrimSpace(value))
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan env file %q: %w", path, err)
	}

	return values, nil
}

// unquote strips a single matching pair of surrounding double or single quotes
// from s, if present. It does not unescape anything inside the quotes.
func unquote(s string) string {
	if len(s) < 2 {
		return s
	}

	first, last := s[0], s[len(s)-1]
	if first != last {
		return s
	}
	if first != '"' && first != '\'' {
		return s
	}

	return s[1 : len(s)-1]
}

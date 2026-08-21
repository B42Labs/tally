// Package envsecret implements the *_FILE convention every service's
// configuration follows: a secret may be given inline in its variable or as a
// path in a companion variable, which is how a Kubernetes Secret volume reaches
// the process without the value appearing in a pod spec.
//
// The normative specification is roadmap/00-conventions.md section 8, which
// asks for one shared helper rather than a copy per service.
package envsecret

import (
	"fmt"
	"os"
	"strings"
)

// Suffix names the companion variable of a secret: the value of
// TALLY_REPORTING_DB_URL is read from the file at TALLY_REPORTING_DB_URL_FILE
// when that variable holds a path.
const Suffix = "_FILE"

// Resolve applies the convention to one variable: when its companion holds a
// path, the file's content becomes the value. Kubernetes writes Secret volumes
// with a trailing newline, so one is trimmed. An empty file is rejected because
// it usually means the secret was never populated, which would otherwise
// surface much later as a failed connection or an authentication failure.
func Resolve(name, value string) (string, error) {
	fileVar := name + Suffix
	path := os.Getenv(fileVar)
	if path == "" {
		return value, nil
	}
	if value != "" {
		return "", fmt.Errorf("set %s or %s, not both", name, fileVar)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", fileVar, err)
	}
	secret := strings.TrimSuffix(string(content), "\n")
	if secret == "" {
		return "", fmt.Errorf("%s: file %s is empty", fileVar, path)
	}
	return secret, nil
}

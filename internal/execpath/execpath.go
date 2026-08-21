// Package execpath resolves external binary paths with env overrides so
// open-source deployments can point at non-standard install locations
// (e.g. LAMBS_SQLITE3_PATH=/usr/local/bin/sqlite3). Without an override
// the bare name is returned and PATH resolution applies, as before.
package execpath

import (
	"os"
	"strings"
)

// Path returns the env-overridden path for the given binary name, or the
// bare name when no override is set. The env key is LAMBS_<NAME>_PATH.
func Path(name string) string {
	key := "LAMBS_" + strings.ToUpper(name) + "_PATH"
	if p := os.Getenv(key); p != "" {
		return p
	}
	return name
}

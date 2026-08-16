package handlers

import (
	"fmt"

	"lambs-server-go/internal/db"
)

const readAllChunk = 500

// readAllMax is a var so tests can shrink it to exercise the cap path.
var readAllMax = 1_000_000

// readAllItems sweeps a table in capped chunks so search/sort sees every row,
// not just the first window (R3-6). Used only when search or sort is active;
// plain browsing keeps the source-side LIMIT/OFFSET path.
//
// Known source limits (documented residuals): REST re-downloads the full
// collection per chunk (no offset param), and the vector source scrolls at
// most 200 points per call — both still terminate, but very large tables are
// O(N²)/lossy respectively. Hitting readAllMax returns an error instead of
// silently truncated rows.
func readAllItems(src db.DataSource, table string) ([]map[string]interface{}, []string, string, error) {
	var all []map[string]interface{}
	var cols []string
	var pk string
	for off := 0; off < readAllMax; off += readAllChunk {
		chunk, c, p, err := src.ReadItems(table, readAllChunk, off)
		if err != nil {
			return nil, nil, "", err
		}
		// The terminating (possibly empty) chunk must not clobber the
		// columns/pk captured from real rows (REST/Mongo return empty
		// cols on an empty page).
		if len(chunk) > 0 {
			cols, pk = c, p
		}
		all = append(all, chunk...)
		if len(chunk) < readAllChunk {
			return all, cols, pk, nil
		}
	}
	return nil, nil, "", fmt.Errorf("表过大（超过 %d 行），请缩小范围后重试", readAllMax)
}

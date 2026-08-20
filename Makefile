# Lambs server — developer entry points (QA round 5: local runs must match CI).
# CI runs `go test -p 1 -race ./...`; -p 1 serializes packages because they
# share one test database (running parallel makes cross-package fixtures
# collide and full-suite runs fail spuriously).

.PHONY: test test-db vet build race check-routes

# Route→test diff (audit tool, not a CI gate): lists handlers whose names
# appear in no _test.go. E2E-mux-only coverage shows up here too — review
# the list by hand before treating an entry as a real gap.
check-routes:
	bash scripts/route-coverage.sh

test:
	go test -p 1 ./...

# Real-postgres integration tests (docker: lambs-pg-test on :5433).
test-db:
	LAMBS_TEST_PG_DSN="postgres://postgres:postgres@127.0.0.1:5433/lambs_test?sslmode=disable" go test -p 1 ./...

vet:
	go vet ./...

race:
	go test -p 1 -race ./...

build:
	go build ./...

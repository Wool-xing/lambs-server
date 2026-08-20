# Lambs server — developer entry points (QA round 5: local runs must match CI).
# CI runs `go test -p 1 -race ./...`; -p 1 serializes packages because they
# share one test database (running parallel makes cross-package fixtures
# collide and full-suite runs fail spuriously).

.PHONY: test test-db vet build race

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

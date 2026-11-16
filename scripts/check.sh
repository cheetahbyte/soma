#!/bin/sh
set -eu
cd "$(dirname "$0")/.."

check() {
	unformatted=$(gofmt -l .)
	if [ -n "$unformatted" ]; then
		printf 'gofmt: these files need formatting:\n%s\n' "$unformatted" >&2
		exit 1
	fi
	go vet ./...
	go build ./...
	go test ./...
}

check
if [ -f cmd/soma/go.mod ]; then
	(cd cmd/soma && check)
fi

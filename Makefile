# dd-worker: CGO_ENABLED=0 for Raspberry Pis, x86 Linux hosts and Apple
# Silicon. `make worker` is the dev build (bare git version); releases are
# `make worker-dist WORKER_VERSION=X.Y.Z` (release.yml, on a vX.Y.Z tag).
WORKER_LDFLAGS = -s -w -X main.version=$$(git describe --always --dirty)
WORKER_BUILD = CGO_ENABLED=0 go build -trimpath -ldflags "$(WORKER_LDFLAGS)"
.PHONY: test vet tidy-check worker worker-cross worker-linux-arm64 worker-linux-amd64 \
  worker-darwin-arm64 worker-dist worker-package-test

test:
	go test ./... -race -count=1

# go vet, then gofmt: any file it lists needs formatting.
vet:
	go vet ./...
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt -l lists:"; echo "$$out"; exit 1; fi

# No personal paths, private hosts or names in the tracked tree.
tidy-check:
	sh scripts/tidy-check.sh

# Release assets in dist/: dd-worker_<V>_<os>_<arch>.tar.gz for each platform,
# install.sh and SHA256SUMS. WORKER_VERSION reaches the script through the
# environment only (exported, never spliced into a command line); the script
# checks its shape. WORKER_RELEASE_BASE (optional) is stamped into the shipped
# install.sh as its download base.
export WORKER_VERSION WORKER_RELEASE_BASE
worker-dist:
	sh packaging/dist.sh dist

# install.sh in a throwaway Podman debian:bookworm container (native arch):
# layout, modes, idempotence, secrets, upgrade/rollback, remove, wrong sum.
worker-package-test:
	sh packaging/test/install_test.sh

worker:
	$(WORKER_BUILD) -o bin/dd-worker ./cmd/dd-worker

worker-cross: worker-linux-arm64 worker-linux-amd64 worker-darwin-arm64

worker-linux-arm64:
	GOOS=linux GOARCH=arm64 $(WORKER_BUILD) -o bin/dd-worker-linux-arm64 ./cmd/dd-worker

worker-linux-amd64:
	GOOS=linux GOARCH=amd64 $(WORKER_BUILD) -o bin/dd-worker-linux-amd64 ./cmd/dd-worker

worker-darwin-arm64:
	GOOS=darwin GOARCH=arm64 $(WORKER_BUILD) -o bin/dd-worker-darwin-arm64 ./cmd/dd-worker

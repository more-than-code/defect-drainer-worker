# Contributing

Thank you for your interest in the Defect Drainer worker. This repository is
licensed under the Apache License 2.0.

## Outside contributions

**We do not merge pull requests from outside contributors yet.** Before we
can, every contributor must sign a contributor agreement. The agreement grants
the copyright holder a licence to your contribution, so the project can keep
offering it under its open-source licence and under a commercial licence. The
agreement is being prepared. Until it is published here, please open an issue
to report a bug or discuss an idea instead of sending a pull request.

Questions go to support@defectdrainer.com.

## Build and test

You need Go 1.25 or newer and `git`. `make worker-package-test` also needs
Podman or Docker.

```sh
make worker                # build bin/dd-worker
make vet                   # go vet ./... and gofmt
make test                  # go test ./... -race -count=1
make tidy-check            # no personal paths or hosts in the tree
make worker-package-test   # install.sh in a throwaway debian:bookworm container
```

CI runs all of these on every push and pull request to `main`.

Some rules the tests enforce:

- **Dependencies.** The module may depend only on the standard library,
  `github.com/coder/websocket` and `golang.org/x/sys` (`boundary_test.go`). A
  new dependency needs a maintainer's agreement first.
- **Environment.** `agentadapter` reads no environment variables, and only
  `git/hostenv.go` may start `git` or `gh`.

## Name

The Defect Drainer name and logo are not licensed under the code licence;
forks must use a different name. See [TRADEMARKS.md](TRADEMARKS.md).

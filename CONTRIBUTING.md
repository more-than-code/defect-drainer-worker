# Contributing

Thanks for helping with the Defect Drainer worker.

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

CI runs all of these on every pull request to `main`.

Some rules the tests enforce:

- **Dependencies.** The module may depend only on the standard library,
  `github.com/coder/websocket` and `golang.org/x/sys` (`boundary_test.go`). A
  new dependency needs a maintainer's agreement first.
- **Environment.** `agentadapter` reads no environment variables, and only
  `git/hostenv.go` may start `git` or `gh`.

## Pull requests

- Keep each pull request to one change, with tests.
- Describe what changes for someone running a worker.
- Changes to `docs/contracts/` change the protocol the server speaks. Open an
  issue first.

## Contributor Licence Agreement

> **DRAFT, pending legal review.** This text is not final. It may change before
> the first non-pre-release version.

To contribute, include this line in your pull request description:

> I have read and agree to the Contributor Licence Agreement in CONTRIBUTING.md.

By doing so, you ("You") agree to the following with Mi Zhou ("the
Maintainer") for every contribution You submit to this repository
("Contribution"):

1. **Copyright licence.** You grant the Maintainer, and recipients of software
   the Maintainer distributes, a perpetual, worldwide, non-exclusive, no-charge,
   royalty-free, irrevocable licence to reproduce, prepare derivative works of,
   publicly display, publicly perform, sublicense and distribute Your
   Contributions and derivative works, under any licence terms, including terms
   other than Apache-2.0.
2. **Patent licence.** You grant the Maintainer, and recipients of software the
   Maintainer distributes, a perpetual, worldwide, non-exclusive, no-charge,
   royalty-free, irrevocable patent licence to make, have made, use, offer to
   sell, sell, import and otherwise transfer the work. This licence covers only
   patent claims You can license that are necessarily infringed by Your
   Contribution alone or combined with the work it was submitted to.
3. **Your right to grant.** Each Contribution is Your original work, or You have
   the right to submit it under this agreement. If Your employer has rights to
   Your work, You have its permission to make the Contribution, or it has
   waived those rights.
4. **Third-party work.** If any part of a Contribution is not Your original
   work, You say so in the pull request. You identify its source and licence.
5. **No other obligations.** You provide Contributions "as is", without
   warranties of any kind. You are not expected to provide support for them.
6. **You keep your copyright.** This agreement grants licences. It does not
   transfer ownership of Your Contributions.

Questions about the CLA go to support@defectdrainer.com.

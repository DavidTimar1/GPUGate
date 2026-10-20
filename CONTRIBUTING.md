# Contributing

GPUGate is a hobby project, and there is no time set aside to maintain it. Contributions are welcome all the same, as pull requests or issues. There is no promise of when either is read or whether a pull request is merged, so a small pull request that says what it fixes and how it was checked has the best chance. Forking it and changing it however you like under the licence is welcome too.

Whether you send a pull request or keep a fork, these are the rules the code was written to:

- **Check every change** with `gofmt -l .`, `go vet ./...` and `go build ./...`. The project has no tests; it was written as a hobby and is meant to fail loudly where it is wrong.
- **One binary, one port, one copy.** The program listens on exactly the addresses named in `GPUGATE_LISTEN` and on one port, and stops rather than moving to another port. `./gate.sh` starts and stops it.
- **Only a fresh provider answer says what is running.** The database records what the program did. "Could not find out" is its own answer and is never treated as "nothing is running".
- **Write the record before the money is spent.** A claim or destroy record is committed before the provider is asked, so a crash still leaves a trace of what may be billing.
- **Never choose for the user.** An empty choice is a complete answer. The mover is the one exception, and only among the machines the user ticked.
- **Never rebuild or publish a serving image.** Models are downloaded onto the rented machine and every serving setting goes on the command line.
- **Credentials are referenced by name, never by value.** The database stores where a secret lives and has no column that can hold one. Nothing logs a key.
- **ASCII only in source.** Code, comments and any string that may be printed are ASCII.
- **Rebuild the page after a change in `ui/src`** with `npm --prefix ui run build`, and commit both `ui/src` and `ui/dist`; CI fails when they differ. There is no framework and no bundler.
- **Bump `VERSION`** with every change: patch for a fix or a document, minor for a new capability, major for a break.
- **Give each setting one home**: `example.env` lists every environment variable the program reads.

## Licence

GNU AGPL-3.0-or-later; see [LICENSE](LICENSE).

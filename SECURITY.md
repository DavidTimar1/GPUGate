# Security policy

## Supported versions

Only the latest release of GPUGate is looked at. Older versions are not patched, so upgrade to the latest release.

## Reporting a vulnerability

Please report security problems privately, never in a public issue. Use GitHub's private vulnerability reporting: "Report a vulnerability" under the repository's Security tab.

Include what you found, how to reproduce it, and the impact you expect. GPUGate is a hobby project with no time set aside to maintain it (see [CONTRIBUTING.md](CONTRIBUTING.md)), so a report may go unanswered and there is no promised time for a fix. Please allow reasonable time before any public disclosure.

## Scope notes

GPUGate runs on your own machine and rents GPU machines with your own provider keys. What matters most:

- anything that lets someone reach the console without the password, or the fixed address `/p/<project>` and `GET /api/projects` without the access key, when one is set;
- anything that makes it rent, keep or fail to destroy a machine nobody asked for, because that spends the user's money;
- anything that writes a credential's value to the database, a log or a page.

How the login, the sessions and the access key work is in [the architecture](docs/architecture.md#the-login).

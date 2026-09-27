# Security policy

## Reporting a vulnerability

Please report suspected vulnerabilities through GitHub's private vulnerability reporting for this repository when it is enabled. If private reporting is unavailable, contact the repository maintainers through GitHub before publishing technical details.

Include the affected version or commit, deployment shape, a concise impact description, and a reproducible case that does not contain real user data or credentials. Do not include commons invites, bearer tokens, private ciphertext, or production database files in a report.

## Security posture

The service defaults to a loopback listener, stores SQLite data with owner-only permissions, uses a cgo-free SQLite implementation, applies protocol and storage limits, and requires TLS for direct non-loopback binds. Operators remain responsible for TLS termination, secret storage, access policy, backups, monitoring, and timely upgrades.

The service has not received an independent security audit. It does not decrypt frames or attachments and cannot verify client-side moderation. A valid commons invite grants access to the corresponding shared scope; protect and rotate that capability if exposed.

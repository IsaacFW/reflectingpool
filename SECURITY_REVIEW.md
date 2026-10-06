# Reflecting Pool security review

**Date:** 2026-10-05

**Repository:** https://github.com/IsaacFW/reflectingpool

**Revision reviewed:** `aa486d7c31c69ec71a70082849a0c1bc734f4cce` (main at retrieval)

**Method:** source review, existing tests, race detector, Go static analysis,
dependency vulnerability scanning, and targeted local vulnerability reproductions.

**Status (2026-10-06):** all five findings are fixed, in the pull request "Security: fix the five findings of the review". The reproductions in `audit_security_test.go` became `attack_test.go` in each package and now assert refusal; `scripts/security_http_probe.py` was replaced by `TestAttackRequestBodyThatNeverArrives`. RP-01 and RP-04 use `internal/safefs`, which opens every folder below a scan root relative to the one above it, refusing links; RP-02 counts a login attempt on arrival and again when it reaches a hashing slot; RP-03 sets a per-request body deadline; RP-05 drops the authority flag and replaces an old authority certificate on start.

## Executive summary

Found **one high-severity and four medium-severity issues**. The most important
problem is the combination of root execution, writable network shares, and
metadata operations that follow symlinks. A user able to modify a share can
redirect privileged metadata writes outside that share without knowing the web
admin password; a subsequent admin action can trigger the write.

The application has useful security controls: strong password hashing, random
setup codes and sessions, hashed session tokens, CSRF checks, strict cookies,
parameterized queries and allowlisted SQL sort expressions, escaped UI text,
and component-by-component symlink protection for content previews. The gaps
are principally in filesystem metadata handling and availability controls.

| ID | Severity | Finding | Prerequisite |
|---|---|---|---|
| RP-01 | High | Metadata reads/writes escape shares through symlinks | Ability to create/replace metadata paths in a writable share; service operation triggers access |
| RP-02 | Medium | Concurrent login requests bypass failed-attempt lockout | Network access to the login endpoint |
| RP-03 | Medium | Incomplete request bodies have no read deadline | Network access to HTTP/HTTPS server |
| RP-04 | Medium | Scanner follows symlinked ancestors after directories are queued | Ability to rename/replace an ancestor of queued directories during a scan |
| RP-05 | Medium, conditional | App TLS key can sign certificates for unrelated domains | App certificate imported as a trusted CA, then private key compromised |

Severity reflects this NAS application's intended deployment. The reproduced
write target is within the service's filesystem namespace: mounted host paths
and writable container paths. This does not establish a container escape.

## RP-01 — Symlink redirection of privileged metadata operations

**Severity:** High

**Category:** CWE-61 / CWE-59, improper link resolution

### Evidence

- [`internal/meta/meta.go:90-105`](https://github.com/IsaacFW/reflectingpool/blob/aa486d7c31c69ec71a70082849a0c1bc734f4cce/internal/meta/meta.go#L90-L105): `os.Stat` and `os.ReadFile` follow links when loading annotations.
- [`internal/meta/meta.go:203-215`](https://github.com/IsaacFW/reflectingpool/blob/aa486d7c31c69ec71a70082849a0c1bc734f4cce/internal/meta/meta.go#L203-L215): `prepare` accepts an existing `.reflection` path without checking that it is a genuine directory beneath the share.
- [`internal/meta/meta.go:296-318`](https://github.com/IsaacFW/reflectingpool/blob/aa486d7c31c69ec71a70082849a0c1bc734f4cce/internal/meta/meta.go#L296-L318): `AddSkips` opens `skipped.jsonl` with append/create flags and follows a final-component symlink.
- [`internal/meta/meta.go:407-424`](https://github.com/IsaacFW/reflectingpool/blob/aa486d7c31c69ec71a70082849a0c1bc734f4cce/internal/meta/meta.go#L407-L424): temporary creation and rename use string paths whose parent components can be symlinks.
- [`Dockerfile:22-24`](https://github.com/IsaacFW/reflectingpool/blob/aa486d7c31c69ec71a70082849a0c1bc734f4cce/Dockerfile#L22-L24): production runs as root.

### Reproduced behavior

Three disposable-fixture tests confirmed:

1. `.reflection -> outside-directory` causes saving a note to write
   `annotations.jsonl`, `INDEX.md`, and `README.txt` into the outside directory.
2. `.reflection/skipped.jsonl -> outside/victim.txt` causes a skip operation to
   append a JSON-encoded, attacker-chosen path to the victim.
3. An annotation-file symlink loads valid JSONL from outside the share.

### Impact and attack conditions

A share writer who can create symlinks can arrange the redirection and wait for
the admin to annotate or skip an item. The file access runs with the service's
root privileges rather than the share writer's permissions. Consequences include
corruption of other shares' metadata, append corruption of arbitrary accessible
files, and metadata-file reads outside the share. JSON encoding limits the append
payload; arbitrary executable code injection was not demonstrated.

Following FIFOs or unbounded special files during metadata reads is also an
availability risk. Those destructive/resource-exhaustion cases were not executed.

### Fix

Anchor metadata operations to a verified share directory descriptor. Resolve
every component without following symlinks, including the share path itself,
`.reflection`, and metadata files. Use directory-relative open/create/rename/
unlink operations, refuse non-regular metadata files, and apply ownership/modes
through descriptors. `openat2` with `RESOLVE_BENEATH | RESOLVE_NO_SYMLINKS`, or
equivalent component-by-component `openat` handling, can enforce confinement
while allowing intended dataset mounts. Consider hardlink aliases too: rejecting
only symlinks does not secure in-place append to a hardlinked file. Stream and
bound metadata reads. A separate `Lstat` check followed by ordinary string-path
I/O leaves a race.

## RP-02 — Queued guesses bypass login lockout

**Severity:** Medium

**Category:** CWE-307, insufficient restriction of authentication attempts

### Evidence

[`internal/auth/auth.go:192-214`](https://github.com/IsaacFW/reflectingpool/blob/aa486d7c31c69ec71a70082849a0c1bc734f4cce/internal/auth/auth.go#L192-L214)
checks lockout before hashing. Requests then block at the hashing semaphore in
[`auth.go:357-359`](https://github.com/IsaacFW/reflectingpool/blob/aa486d7c31c69ec71a70082849a0c1bc734f4cce/internal/auth/auth.go#L357-L359).
Failures are counted only after verification. Queued requests do not recheck
lockout when a hashing slot becomes available.

### Reproduced behavior

A test held the two hash slots, submitted 32 wrong-password logins from one
address, waited until all had entered verification, and released the slots.
**All 32 guesses were evaluated**, despite the configured five-failure threshold.
The resulting lockout affected later arrivals but did not reject queued guesses.

### Impact

A parallel burst defeats the advertised per-address attempt limit and keeps
hash workers busy. Argon2 still makes each guess expensive and the semaphore
limits simultaneous hashing memory, but neither bounds the backlog nor prevents
queued guesses from being tried after lockout.

### Fix

Atomically reserve an attempt before accepting password-verification work.
Bound per-address and global pending work, reject excess requests promptly, and
make waiting context-aware. Recheck lockout after admission to the hash worker;
combine that with atomic accounting so concurrent in-flight requests cannot
indefinitely exceed the allowance. Add an API-level concurrent-login regression
test as well as service-level coverage.

## RP-03 — No request-body read deadline

**Severity:** Medium

**Category:** CWE-400, uncontrolled resource consumption

### Evidence

[`cmd/reflectingpool/main.go:213-219`](https://github.com/IsaacFW/reflectingpool/blob/aa486d7c31c69ec71a70082849a0c1bc734f4cce/cmd/reflectingpool/main.go#L213-L219)
sets `ReadHeaderTimeout` and `IdleTimeout`, but no body-read deadline.
[`internal/server/server.go:298-305`](https://github.com/IsaacFW/reflectingpool/blob/aa486d7c31c69ec71a70082849a0c1bc734f4cce/internal/server/server.go#L298-L305)
decodes JSON directly from the request body.

### Reproduced behavior

Built the real application and started it on loopback with a disposable empty
pool. Sent complete headers for `POST /api/login`, declared a 1,024-byte body,
and sent only `{`. The server left the connection open with no response after
12 seconds, beyond its 10-second header timeout.

### Impact

Unauthenticated clients can retain active connections and handler goroutines
by withholding body bytes. The 1 MiB body-size limit constrains bytes, not elapsed
time. `IdleTimeout` concerns waiting for the next request, not reading the current
one. Connection/resource exhaustion was not attempted. A reverse proxy with
appropriate body timeouts can mitigate exposure; direct serving is the default.

### Fix

Set a finite request read timeout suitable for the small JSON API, or enforce
body-read deadlines with `http.ResponseController`. Bound concurrent requests
and pending authentication work. Choose any response-write deadlines carefully
because legitimate content previews stream large media files.

## RP-04 — Scan confinement fails for replaced ancestors

**Severity:** Medium

**Category:** CWE-61 / CWE-367, link resolution and time-of-check/time-of-use

### Evidence

[`internal/scan/scan.go:499-521`](https://github.com/IsaacFW/reflectingpool/blob/aa486d7c31c69ec71a70082849a0c1bc734f4cce/internal/scan/scan.go#L499-L521)
queues directories by full string path. The later open in
[`scan.go:538-548`](https://github.com/IsaacFW/reflectingpool/blob/aa486d7c31c69ec71a70082849a0c1bc734f4cce/internal/scan/scan.go#L538-L548)
uses `O_NOFOLLOW`, which rejects only a symlink in the final component.

### Reproduced behavior

Created `root/share/queued`, prepared the work item corresponding to a queued
directory, renamed `share`, and replaced it with a symlink to an outside
directory containing `queued/private-name.txt`. Calling the actual queued
directory reader indexed that outside file's metadata.

### Impact

A share writer can cause a running scan to traverse outside its configured
roots, persist outside filenames and sizes, and consume resources walking an
unintended tree. Metadata is exposed through the authenticated index, not directly
to an unauthenticated share writer. This reproduction does **not** establish an
outside-file-content preview bypass: the preview opener separately checks each
path component with `O_NOFOLLOW`.

### Fix

Resolve queued directories relative to a pinned scan-root descriptor with
no-symlink/beneath semantics, or queue safe directory handles within a bounded
descriptor budget. Validate the opened directory identity and mount/exclusion
policy before reading it. Reuse the confinement model already implemented for
content reads in `internal/core/safeopen.go`.

## RP-05 — Unconstrained CA certificate used as app server identity

**Severity:** Medium, conditional

**Category:** excessive cryptographic trust scope

### Evidence

[`internal/server/tls.go:52-57`](https://github.com/IsaacFW/reflectingpool/blob/aa486d7c31c69ec71a70082849a0c1bc734f4cce/internal/server/tls.go#L52-L57)
sets `KeyUsageCertSign` and `IsCA: true`, explicitly to support importing the
certificate into a browser or device trust store. There are no name constraints.

### Reproduced behavior

Generated an app certificate in a temporary directory, used its private key to
sign a server certificate for `unrelated.example`, and verified that certificate
successfully with the app certificate as the trusted root.

### Impact

If the owner installs this certificate as a CA trust anchor, compromise of the
app's TLS key can enable impersonation of unrelated HTTPS domains to those
devices, given a suitable interception position. This requires CA trust-store
installation; merely accepting the site's browser certificate exception does
not establish the same trust. No private-key compromise was demonstrated.

### Fix

Generate an ordinary self-signed server leaf with `IsCA: false` and no
`KeyUsageCertSign`. For warning-free access, use a server leaf signed by a
separate trusted CA whose signing key is not accessible to the app, or a
publicly trusted certificate. Remove any previously imported app CA from trust
stores when migrating.

## Verification and artifacts

Executed successfully:

```sh
go test ./...                          # baseline, before audit tests
go vet ./...
go test -race ./...                    # includes all added audit tests
go test -run TestAudit -v ./internal/meta ./internal/auth ./internal/scan ./internal/server
go run golang.org/x/vuln/cmd/govulncheck@latest -show verbose ./...
```

Dependency scan: **zero reachable known vulnerabilities**, zero additional
imported-package findings. One required-module advisory, **GO-2026-5932**, affects
`golang.org/x/crypto/openpgp`; this application does not import that package.
An advisory-database result does not validate the application-specific controls
covered by the findings above.

A targeted credential-pattern search of the retrieved Git history produced no
matches for common AWS access-key IDs, GitHub token prefixes, or PEM private-key
headers. This is not an exhaustive secret scan.

Audit additions to the local clone:

- `internal/meta/audit_security_test.go` — three metadata-link reproductions.
- `internal/auth/audit_security_test.go` — concurrent lockout reproduction.
- `internal/scan/audit_security_test.go` — queued-directory traversal reproduction.
- `internal/server/audit_security_test.go` — unrelated-domain CA signing reproduction.

These tests intentionally assert the current vulnerable behavior. When applying
fixes, convert them into regression tests that assert rejection/confinement.
Production source was not modified.

`scripts/security_http_probe.py` reproduces the production-server body-timeout
finding. From the repository root, run:

```sh
go build -o /tmp/reflectingpool-audit ./cmd/reflectingpool
RP_AUDIT_BINARY=/tmp/reflectingpool-audit python3 scripts/security_http_probe.py
```

It binds loopback, uses a disposable pool/data directory, and terminates the
server afterward. Like the Go audit tests, it asserts the vulnerable behavior.

## Scope and remediation order

Reviewed API routing, authentication/session/CSRF controls, filesystem traversal
and metadata persistence, preview response handling, SQL construction, UI text
and URL handling, TLS generation, container configuration, and image workflow.
The review did not exercise a live NAS, ZFS device permissions, production proxy
configuration, browser-engine exploitation, container-package CVEs, or a full
third-party JavaScript vulnerability assessment.

Recommended order:

1. **Fix RP-01 first**, before relying on isolation from untrusted share writers.
2. Fix RP-02 and RP-03 together to bound unauthenticated request work.
3. Fix RP-04 using the same filesystem confinement primitives as RP-01.
4. Replace the app CA identity and document migration for owners who imported it.

For a running instance pending RP-01 remediation, `RP_READ_ONLY=1` disables the
vulnerable pool write operations. A read-only pool bind mount adds enforcement at
the mount layer. Metadata reads and scanner traversal still require fixes.

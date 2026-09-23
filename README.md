<p align="center">
  <img src="assets/logo.svg" alt="envoke" width="392">
</p>

# envoke (`secrets-entrypoint`)

A single static Go binary that replaces the classic shell-based Kubernetes
entrypoint pattern:

```sh
command: ["/bin/sh", "-c"]
args:
  - |
    . /vault/secrets/config
    echo "=== Loaded Secret Keys ==="
    awk -F'=' '/^export/ {sub(/^export /, ""); print $1}' /vault/secrets/config
    echo "=========================="
    exec node main.js
```

It loads secrets from one or more files into the process environment,
prints which keys were loaded (never values), best-effort reports the
outcome to the Kubernetes Events API, and then `execve`s straight into the
real application — so the real app inherits PID 1 and receives signals
(`SIGTERM`, etc.) directly. No wrapper process is left running.

Written in Go's standard library only — no third-party dependencies.

## CLI

```
secrets-entrypoint [flags] -- <command> [args...]
```

Everything after `--` is resolved via `$PATH` and exec'd with its argv
exactly as given (`argv[0]` unmodified).

| Flag       | Env var                  | Default                 | Meaning                                   |
|------------|---------------------------|--------------------------|--------------------------------------------|
| `-config`  | `SECRETS_CONFIG_PATH`     | `/vault/secrets/config`  | Repeatable literal path or glob pattern (colon-separated in the env var) |
| `-format`  | `SECRETS_CONFIG_FORMAT`   | `auto`                   | `auto`, `shell`, or `json` — forces the format for every `-config` entry |
| `-quiet`   | `SECRETS_QUIET`           | `false`                  | Suppress the stdout banner (`1`/`true` = on) |

Precedence for every setting: **CLI flag > env var > default**.

Not a flag — injected via the pod's Downward API:

| Env var   | Purpose                                              |
|-----------|-------------------------------------------------------|
| `POD_UID` | Required for Kubernetes Event reporting (`fieldRef: metadata.uid`) |

## Config file formats

**Shell** (default, auto-detected): optional leading `export `, `KEY=VALUE`,
values may be wrapped in matching single/double quotes (stripped), blank
lines and full-line `#` comments are ignored. No variable expansion,
command substitution, multi-line values, or inline comments.

**JSON**: a flat top-level object. Strings are used as-is; numbers, bools,
and `null` are converted to their string form (`"3"`, `"true"`, `""`). A
nested object or array value is a hard parse error.

Format auto-detection (only when `-format`/`SECRETS_CONFIG_FORMAT` is
`auto`): a `.json` filename extension wins outright; otherwise the first
non-whitespace byte of the file (`{` → JSON, else shell).

## Glob patterns

Any `-config` entry containing `*`, `?`, or `[...]` is expanded with the
standard library's `path/filepath.Glob` (no recursive `**`). Matches are
processed in lexical order; entries are processed in the order given on
the command line / in `SECRETS_CONFIG_PATH`. Later-read keys always
override earlier ones. A pattern matching zero files is treated exactly
like a missing literal path (soft, not an error). Directories in a glob
match set are silently skipped.

## Failure model

- **Soft** (continue, still exec the target command): a literal path that
  doesn't exist, or a glob pattern matching zero files.
- **Hard** (stop, do *not* exec, exit non-zero): a file that exists but
  fails to parse — invalid JSON, a shell line with no `=`, or a JSON value
  that isn't string/number/bool/null. A clear, specific error (file,
  line/key, what was wrong) is printed to stderr. No Kubernetes Event is
  sent for this case — it's a startup failure that should surface as a
  normal pod crash/log.

## Kubernetes Event reporting

Best-effort only — never blocks, never changes the exit code or exec
outcome:

- If `/var/run/secrets/kubernetes.io/serviceaccount/token` is absent, the
  entire event step is skipped silently (e.g.
  `automountServiceAccountToken: false`).
- If `POD_UID` is unset, the event step is skipped with one stderr notice
  (an Event without the real UID won't show up under `kubectl describe
  pod`).
- Any other failure (network, RBAC 403, etc.) prints one warning line to
  stderr and execs anyway.
- Pod name comes from `os.Hostname()`. **Known limitation:** this breaks
  under `hostNetwork: true`, where the hostname is the node's, not the
  pod's. This is not auto-detected or worked around.
- Namespace is read from
  `/var/run/secrets/kubernetes.io/serviceaccount/namespace`.
- On success: `reason: SecretsLoaded`, `type: Normal`, message like
  `"found 2 keys (DB_PASSWORD, API_KEY)"` (truncated with `"... and N
  more"` if the full list would exceed the Events API message budget).
- If nothing was found anywhere: `reason: SecretsNotFound`, `type:
  Warning`, message lists every configured path/pattern that was searched.

Requires an RBAC `Role` granting `create` on `events` in the pod's own
namespace, bound to the pod's `ServiceAccount` (see below).

## Explicit non-goals

- Shell variable expansion / command substitution inside values
- Recursive glob (`**`)
- `client-go` or any Kubernetes SDK dependency
- Self-lookup of the pod via `GET .../pods/{name}` to discover its own UID
- Retry/backoff waiting for a secrets file to eventually appear (that's
  Vault Agent's job)

## Build

```sh
CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o secrets-entrypoint .
```

Go toolchain is pinned via `.mise.toml` (`go 1.26`); run `mise install`
before building if you use [mise](https://mise.jdx.dev/).

## Test

```sh
go test ./...
```

## Getting the binary into your application image

The binary is built with `CGO_ENABLED=0` against Go's standard library
only, so it's a fully static executable (`ldd` reports "not a dynamic
executable") with zero dependency on the base image's libc. It drops
unmodified into musl images (Alpine), glibc images (Debian/Ubuntu), or
`scratch` — verified by building it into both an `alpine:3.20` and a
`debian:12-slim` image and running it in each.

The canonical way to consume it is this repo's own [Dockerfile](Dockerfile):
build it once, publish it, and pull the binary into any application image
with a multi-stage `COPY --from`. No application repo needs a
`golang:1.26-alpine` builder stage of its own for this binary — that
approach is not what this project documents or supports.

Build and tag it once (locally, or push it to a registry your other
builds can reach):

```sh
docker build -t secrets-entrypoint:latest .
# or, tagged for a registry:
docker build -t my-registry.example.com/secrets-entrypoint:1.0.0 .
docker push my-registry.example.com/secrets-entrypoint:1.0.0
```

Then any application Dockerfile pulls the binary straight out of it, no
Go toolchain required:

```dockerfile
FROM node:20-alpine
COPY --from=my-registry.example.com/secrets-entrypoint:1.0.0 /usr/local/bin/secrets-entrypoint /usr/local/bin/secrets-entrypoint
WORKDIR /app
COPY dist/ ./dist/

ENTRYPOINT ["/usr/local/bin/secrets-entrypoint", "--"]
CMD ["node", "dist/main.js"]
```

(`docker buildx build --platform linux/amd64,linux/arm64 ...` cross-builds
this image for multiple architectures — `TARGETOS`/`TARGETARCH` in the
Dockerfile are wired up for that automatically.)

If you just want the raw binary file on disk (no consuming Dockerfile
involved), extract it without ever running the container:

```sh
cid=$(docker create secrets-entrypoint:latest)
docker cp "$cid:/usr/local/bin/secrets-entrypoint" ./secrets-entrypoint
docker rm "$cid"
```

No Helm `command:`/`args:` override is needed — the image's own
`ENTRYPOINT`/`CMD` pairing already does the right thing. `kubectl run` /
`docker run <image> node other.js` still overrides `CMD` normally, and
`secrets-entrypoint` execs whatever it's given.

## Example Helm `env:` block + RBAC

```yaml
# values / deployment template
spec:
  serviceAccountName: my-app
  containers:
    - name: my-app
      image: my-registry/my-app:latest
      env:
        - name: SECRETS_CONFIG_PATH
          value: "/vault/secrets/config:/vault/secrets/*.json"
        - name: POD_UID
          valueFrom:
            fieldRef:
              fieldPath: metadata.uid
```

```yaml
# rbac.yaml — required for Kubernetes Event reporting
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: my-app-event-reporter
  namespace: my-namespace
rules:
  - apiGroups: [""]
    resources: ["events"]
    verbs: ["create"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: my-app-event-reporter
  namespace: my-namespace
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: my-app-event-reporter
subjects:
  - kind: ServiceAccount
    name: my-app
    namespace: my-namespace
```

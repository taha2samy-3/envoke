# syntax=docker/dockerfile:1
#
# This image's only job is to hold the statically-linked secrets-entrypoint
# binary at a fixed path, so any other Dockerfile can pull it in with a
# multi-stage COPY --from=<this-image> -- no registry client, no package
# manager, nothing else required.
#
# Because the binary is built with CGO_ENABLED=0 against Go's standard
# library only, it has zero dynamic dependencies (no libc, no third-party
# .so's) and drops into ANY base image unmodified -- alpine (musl),
# debian/ubuntu (glibc), distroless, or scratch.

# ---------------------------------------------------------------------
# Stage 1: builder — compiles the static binary for the target platform.
# The builder itself always runs on the *build* machine's native
# platform (--platform=$BUILDPLATFORM): Go cross-compiles for whatever
# TARGETOS/TARGETARCH buildx requests without needing QEMU emulation to
# execute the builder stage, which is what makes multi-arch builds
# (linux/amd64, linux/arm64, ...) fast here. BUILDPLATFORM/TARGETOS/
# TARGETARCH are set automatically by `docker buildx build --platform`.
# ---------------------------------------------------------------------
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder


# Deliberately no default value here: buildx injects the real target
# platform's TARGETOS/TARGETARCH into this stage automatically, but only
# as long as the ARG itself is undefaulted -- giving it a hardcoded
# default (e.g. "amd64") silently pins every platform's build to that
# default instead. The ":-linux"/":-amd64" shell fallbacks in the RUN
# line below cover plain `docker build` (no buildx --platform), where
# these ARGs are never populated at all.
ARG TARGETOS
ARG TARGETARCH

WORKDIR /src
COPY go.mod ./
COPY *.go ./

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags="-s -w" -o /out/secrets-entrypoint .

# ---------------------------------------------------------------------
# Stage 2: final — nothing but the binary. `scratch` has no shell, no
# libc, no package manager; that's the point -- there is nothing here
# that could make the binary behave differently depending on what it's
# copied into.
# ---------------------------------------------------------------------
FROM scratch AS final

LABEL org.opencontainers.image.title="secrets-entrypoint" \
      org.opencontainers.image.description="Static Go binary; loads Vault-injected secrets into the env and execve's into the real app. Distribution image: COPY --from this image, do not run it directly in production."

COPY --from=builder /out/secrets-entrypoint /usr/local/bin/secrets-entrypoint

ENTRYPOINT ["/usr/local/bin/secrets-entrypoint"]

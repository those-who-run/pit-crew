# The small image, for the no-model steps: pit-crew and twr-claude, built in Go's
# image, run from Debian's slim image with git and CA certificates, as a non-root
# user. image.yml builds and smoke-tests it on every PR; a release builds it again
# and image-publish.yml pushes that build. Both base images are pinned by the digest
# of their multi-platform index (the tag beside it is for people); Renovate bumps
# the tag and the digest together.
#
# Not distroless, which the template starts from: the fetch and publish steps run
# git, which distroless's static image does not have. Nothing else is installed.

FROM docker.io/library/golang:1.27.4-trixie@sha256:b54b2639de8c1bfe538ec5ffc45f29b1932235ac5f6d63c059853646677112d9 AS build
WORKDIR /src
# The modules first, so a change to the code alone reuses this layer.
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
# The version the binaries report (`-version`): `dev` unless the build passes one,
# as `docker build --build-arg VERSION=X.Y.Z .` does. image.yml in
# those-who-run/.github passes VERSION=X.Y.Z when it builds on a release tag.
ARG VERSION=dev
# -trimpath keeps the build machine's paths out of the binaries.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/pit-crew ./cmd/pit-crew \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/twr-claude ./cmd/twr-claude

FROM docker.io/library/debian:trixie-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2
# git for the clone and the push, CA certificates for GitHub. The package lists are
# removed so the image carries no apt state; versions are Debian's at the base
# image's digest.
RUN DEBIAN_FRONTEND=noninteractive apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends git ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && groupadd --gid 65532 nonroot \
 && useradd --uid 65532 --gid 65532 --create-home --home-dir /home/nonroot --shell /usr/sbin/nologin nonroot
# git's global configuration, in place of a home's: /work/repo and /work/agent are
# emptyDir mount points owned by root, and a clone into an existing directory
# keeps it as the repository root, so git as nonroot refuses them as "dubious
# ownership" unless they are safe. GIT_CONFIG_GLOBAL below points git here.
RUN mkdir -p /etc/pit-crew \
 && printf '[safe]\n\tdirectory = /work/repo\n\tdirectory = /work/agent\n' > /etc/pit-crew/gitconfig
COPY --from=build /out/pit-crew /pit-crew
COPY --from=build /out/twr-claude /twr-claude
# The same numeric user as distroless's nonroot, so a runtime can check it isn't
# root. HOME is writable only when the manifest mounts it; git needs no home.
# GIT_CONFIG_NOSYSTEM keeps git from reading /etc/gitconfig, so the image's
# package configuration never reaches a step; GIT_CONFIG_GLOBAL is independent of
# it, and names the one file git reads.
USER 65532:65532
ENV HOME=/home/nonroot GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/etc/pit-crew/gitconfig
ENTRYPOINT ["/pit-crew"]

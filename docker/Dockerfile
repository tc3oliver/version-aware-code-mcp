# Images for the compose stack: one for Zoekt, one carrying vacmcp and the
# codebase-memory-mcp binary it drives.
#
# Every base image is pinned by digest and every fetched binary by version,
# because a stack that silently moves to a newer Zoekt or CBM is a stack whose
# answers cannot be reproduced. CBM is fetched here rather than committed: the
# binary is not vendored into this repository, and the archive is checked
# against the SHA-256 its release publishes before anything is unpacked. Its own
# install.sh is deliberately not run — it rewrites account-wide agent
# configuration, and all that is wanted here is the binary.

########################  build vacmcp  ########################

FROM golang:1.26.5-trixie@sha256:98988b42f3293b627bf07c884ff17181a59501769cd8c06c7ba901e0ce2c9853 AS vacmcp-build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/vacmcp ./cmd/vacmcp

########################  build zoekt  ########################

FROM golang:1.26.5-trixie@sha256:98988b42f3293b627bf07c884ff17181a59501769cd8c06c7ba901e0ce2c9853 AS zoekt-build

# The commit decision-3 pins. Only the two commands this stack uses are built:
# one to index the repository, one to serve the index.
ARG ZOEKT_VERSION=v0.0.0-20260807124253-c4d8f3537f7a
RUN CGO_ENABLED=0 GOBIN=/out go install \
	"github.com/sourcegraph/zoekt/cmd/zoekt-git-index@${ZOEKT_VERSION}" \
	"github.com/sourcegraph/zoekt/cmd/zoekt-webserver@${ZOEKT_VERSION}"

########################  fetch codebase-memory-mcp  ########################

FROM golang:1.26.5-trixie@sha256:98988b42f3293b627bf07c884ff17181a59501769cd8c06c7ba901e0ce2c9853 AS cbm-fetch

# The portable Linux build is the fully static one; the plain linux archive
# links glibc 2.38+ and would tie this image to a base new enough to have it.
#
# The digests are the ones the v0.10.1 release publishes in checksums.txt. They
# are written per architecture because the archive differs per architecture: a
# single digest here would either fail on one of them or, worse, be skipped.
ARG CBM_VERSION=0.10.1
ARG CBM_SHA256_amd64=97c6580a13d772d040e936584f3c5234586ab03f31a77354af8a763851a39a7f
ARG CBM_SHA256_arm64=ad457b306fa5bf5a1b97d160eeb867260a7946018097222a622fb14e896c6c72
ARG TARGETARCH
RUN set -eux; \
	case "$TARGETARCH" in \
	amd64) sha="$CBM_SHA256_amd64" ;; \
	arm64) sha="$CBM_SHA256_arm64" ;; \
	*) echo "no codebase-memory-mcp build for $TARGETARCH" >&2; exit 1 ;; \
	esac; \
	archive="codebase-memory-mcp-linux-${TARGETARCH}-portable.tar.gz"; \
	curl -fsSL -o /tmp/cbm.tar.gz \
		"https://github.com/DeusData/codebase-memory-mcp/releases/download/v${CBM_VERSION}/${archive}"; \
	echo "${sha}  /tmp/cbm.tar.gz" | sha256sum -c -; \
	tar -xzf /tmp/cbm.tar.gz -C /tmp; \
	install -D -m 0755 /tmp/codebase-memory-mcp /out/codebase-memory-mcp; \
	/out/codebase-memory-mcp --version

########################  demo repository and configuration  ########################

# The fixture repository of doc-1 §15: three branches whose Process() calls a
# different handler, which is what makes version isolation observable in this
# stack. It is generated here rather than mounted from the host, so the stack
# stands on its own; both runtime images copy this one stage, so they hold the
# same repository down to the commit SHAs and cannot disagree about which
# revision a context names.
FROM debian:trixie-slim@sha256:3a39a0592364683e6bab97937b72cad5a8fa6dcbbee90edb3bb48c7f8e94f258 AS demo-repo

RUN apt-get update \
	&& apt-get install -y --no-install-recommends git ca-certificates \
	&& rm -rf /var/lib/apt/lists/*

# The directory name is the repository name Zoekt derives from it, and the
# search adapter only accepts a match Zoekt reports under the configured
# repository. Renaming this without renaming it in config.yaml.in would make
# every search return nothing.
COPY testdata/gen-versioned-demo-repo.sh /src/testdata/gen-versioned-demo-repo.sh
RUN /src/testdata/gen-versioned-demo-repo.sh \
	&& mv /src/testdata/versioned-demo-repo /srv/versioned-demo-repo

# The revisions are resolved from the repository just built rather than written
# down, for the reason testdata/prepare-fixture.sh gives: a configuration naming
# a stale revision is exactly the mismatch this server exists to catch.
COPY docker/config.yaml.in /src/config.yaml.in
RUN set -eux; \
	sed \
		-e "s|@REVISION_V1@|$(git -C /srv/versioned-demo-repo rev-parse refs/heads/release/v1)|" \
		-e "s|@REVISION_V2@|$(git -C /srv/versioned-demo-repo rev-parse refs/heads/release/v2)|" \
		/src/config.yaml.in >/src/config.yaml

########################  shared runtime  ########################

FROM debian:trixie-slim@sha256:3a39a0592364683e6bab97937b72cad5a8fa6dcbbee90edb3bb48c7f8e94f258 AS runtime

# git is what vacmcp resolves revisions with and what CBM needs a checkout from;
# curl is what the compose healthcheck probes Zoekt with.
RUN apt-get update \
	&& apt-get install -y --no-install-recommends ca-certificates curl git \
	&& rm -rf /var/lib/apt/lists/*

RUN useradd --create-home --uid 10001 vacmcp
COPY --from=demo-repo --chown=vacmcp:vacmcp /srv/versioned-demo-repo /srv/versioned-demo-repo

# The CBM service adds worktrees under /srv, so the directory itself has to
# belong to the user the containers run as.
RUN chown vacmcp:vacmcp /srv

########################  zoekt  ########################

FROM runtime AS zoekt

COPY --from=zoekt-build /out/zoekt-git-index /out/zoekt-webserver /usr/local/bin/
RUN install -d -o vacmcp -g vacmcp /data/zoekt-index

USER vacmcp
EXPOSE 6070

########################  vacmcp, and the CBM indexer that shares its image  ########################

FROM runtime AS vacmcp

COPY --from=cbm-fetch /out/codebase-memory-mcp /usr/local/bin/codebase-memory-mcp
COPY --from=vacmcp-build /out/vacmcp /usr/local/bin/vacmcp
COPY --from=demo-repo /src/config.yaml /etc/vacmcp/config.yaml
COPY docker/cbm-index.sh /usr/local/bin/cbm-index.sh

# CBM keeps one SQLite graph per project under $HOME/.cache; that directory is
# where the compose stack mounts the shared graph store, so it has to exist and
# belong to the user before the volume is created from it.
RUN install -d -o vacmcp -g vacmcp /home/vacmcp/.cache/codebase-memory-mcp

USER vacmcp
EXPOSE 8080
CMD ["vacmcp", "serve", "--config", "/etc/vacmcp/config.yaml", "--address", "0.0.0.0:8080"]

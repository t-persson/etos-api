FROM golang:1.26-alpine AS build
WORKDIR /tmp/executionspace

# Cache dependencies early
RUN apk add --no-cache make=4.4.1-r4
COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

# Only copy what's needed for building the binary to speed up the build.
# The more likely to change is placed at the bottom so that the cache is
# not invalidated too early.
COPY Makefile Makefile
COPY cmd/executionspace cmd/executionspace
COPY internal/ internal/
COPY pkg/ pkg/
COPY .git/ .git/

RUN make executionspace

FROM alpine:3.17.3
ARG TZ
ENV TZ=$TZ

LABEL org.opencontainers.image.source=https://github.com/eiffel-community/etos-api
LABEL org.opencontainers.image.authors=etos-maintainers@googlegroups.com
LABEL org.opencontainers.image.licenses=Apache-2.0

RUN apk add --no-cache tzdata=2024a-r0
ENTRYPOINT ["/app/executionspace"]

COPY --from=build /tmp/executionspace/bin/executionspace /app/executionspace

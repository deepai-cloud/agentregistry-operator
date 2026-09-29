# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.25.4 AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags='-s -w' -o /out/operator ./cmd/operator && \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags='-s -w' -o /out/gateway ./cmd/gateway

FROM scratch
LABEL org.opencontainers.image.licenses="MIT"
COPY LICENSE /LICENSE
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/operator /operator
COPY --from=build /out/gateway /gateway
USER 1001:1001
EXPOSE 8080
ENTRYPOINT ["/operator"]

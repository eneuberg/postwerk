# syntax=docker/dockerfile:1

# Build stage: static binary, no cgo (O2).
FROM golang:1.27@sha256:ec47ea3725fea0cc88f6de94919aef19f40f12fc1ec0e2d1b3426dbf15ecca04 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/postwerk ./cmd/postwerk \
    && mkdir -p /out/root/data && chmod 0700 /out/root/data

# Final stage: the binary and CA certificates, nothing else. No shell.
FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/postwerk /postwerk
# /data is owned by the runtime user (mode 0700), so a fresh named volume
# mounted there is writable by it only. A bind mount or other volume must be
# writable by uid 65532.
COPY --from=build --chown=65532:65532 /out/root/ /
USER 65532:65532
EXPOSE 2525 8080
ENTRYPOINT ["/postwerk"]
CMD ["serve"]

# Server-Image. Bauen im Repo-Root:
#   docker build -f deploy/Dockerfile -t mat-server .
# Starten:
#   docker run -d -p 8080:8080 -e MAT_AGENT_TOKEN=... -v mat-data:/data mat-server

FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY proto ./proto
COPY server ./server
COPY web ./web
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o /out/mat-server ./server/cmd/mat-server \
 && mkdir -p /out/data

# Minimales Image ohne Shell, läuft als unprivilegierter Benutzer (UID 65532).
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/mat-server /usr/local/bin/mat-server
COPY --from=build --chown=65532:65532 /out/data /data

ENV MAT_LISTEN=:8080 \
    MAT_DB=/data/mat.db
VOLUME /data
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/mat-server"]

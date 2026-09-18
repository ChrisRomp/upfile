# syntax=docker/dockerfile:1

FROM node:24-alpine AS web-build
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN --mount=type=secret,id=npmrc,target=/root/.npmrc \
    --mount=type=tmpfs,target=/root/.npm \
    npm ci
COPY web/ ./
RUN npm run build

FROM golang:1.27-alpine AS go-build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY web/embed.go ./web/embed.go
COPY --from=web-build /src/web/dist ./web/dist
RUN CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags="-s -w" -o /out/upfile ./cmd/upfile \
    && mkdir -p /runtime/data \
    && chown 65532:65532 /runtime/data \
    && chmod 0700 /runtime/data

FROM gcr.io/distroless/static-debian12:nonroot
LABEL org.opencontainers.image.licenses="AGPL-3.0-only" \
    org.opencontainers.image.source="https://github.com/ChrisRomp/upfile"
COPY LICENSE /LICENSE
COPY --from=go-build --chown=65532:65532 /runtime/ /
COPY --from=go-build /out/upfile /upfile
ENV UPFILE_DATA_DIR=/data \
    UPFILE_PUBLIC_ADDR=:8080 \
    UPFILE_ADMIN_ADDR=:8081 \
    TMPDIR=/run/upfile \
    SQLITE_TMPDIR=/run/upfile
USER 65532:65532
WORKDIR /data
EXPOSE 8080 8081
ENTRYPOINT ["/upfile"]

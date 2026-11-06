# Two stages, and almost nothing in the second: one program, the certificates it needs to
# ask GitHub about updates, and an empty data folder. No shell, no package manager, nothing
# else to patch. Run it with a read-only filesystem; it writes only to /data.
FROM docker.io/library/golang:1.27.3-alpine@sha256:0fd028db9bdd4f01657bc8047c209b04d8ce19b853d776bd6f1e76a41729b936 AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/cabinet-linkup ./cmd/cabinet-linkup
RUN mkdir -p /out/data && chown 65532:65532 /out/data

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/cabinet-linkup /cabinet-linkup
COPY --from=build --chown=65532:65532 /out/data /data
USER 65532:65532
VOLUME /data
EXPOSE 7845/tcp 7846/tcp 3478/udp 49160-49200/udp
ENTRYPOINT ["/cabinet-linkup"]

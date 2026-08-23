FROM --platform=$BUILDPLATFORM golang:1.26.4-trixie@sha256:68b7145ec43d1820b9a56704554b53d1520aa2a15cb5233e374188a31b2a1bce AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -buildvcs=false -trimpath -ldflags="-s -w -X main.version=$VERSION" -o /out/swarrow ./cmd/swarrow

FROM gcr.io/distroless/static-debian13:nonroot@sha256:f7f8f729987ad0fdf6b05eeeae94b26e6a0f613bdf46feea7fc40f7bd72953e6

COPY --from=build /out/swarrow /usr/local/bin/swarrow

USER 65532:65532
ENTRYPOINT ["/usr/local/bin/swarrow"]

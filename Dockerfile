# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.26.4-alpine AS build

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG GIT_COMMIT=unknown
ARG BUILD_TIME=unknown

WORKDIR /src

RUN apk add --no-cache ca-certificates git

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags="-s -w -X github.com/redentordev/tako-cli/cmd.Version=$VERSION -X github.com/redentordev/tako-cli/cmd.GitCommit=$GIT_COMMIT -X github.com/redentordev/tako-cli/cmd.BuildTime=$BUILD_TIME" \
    -o /out/tako .

FROM alpine:3.22

RUN apk add --no-cache ca-certificates bash curl docker-cli git openssh-client tar gzip

COPY --from=build /out/tako /usr/local/bin/tako

ENTRYPOINT ["tako"]

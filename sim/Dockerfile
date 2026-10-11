FROM golang:1.27.2 AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/stagely-sim ./cmd/stagely-sim

FROM docker:29.8.1-cli
RUN apk add --no-cache git git-daemon
COPY --from=build /out/stagely-sim /usr/local/bin/stagely-sim
VOLUME /data
ENTRYPOINT ["stagely-sim", "-data-dir", "/data"]

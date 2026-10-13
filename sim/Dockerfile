FROM golang:1.27.2 AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/elprovo-sim ./cmd/elprovo-sim

FROM docker:29.8.1-cli
RUN apk add --no-cache git git-daemon
COPY --from=build /out/elprovo-sim /usr/local/bin/elprovo-sim
VOLUME /data
ENTRYPOINT ["elprovo-sim", "-data-dir", "/data"]

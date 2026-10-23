FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /odb ./cmd/odb

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata && adduser -D -u 10001 odb && mkdir /data && chown odb /data
USER odb
COPY --from=build /odb /usr/local/bin/odb
ENV ODB_ADDR=:8790 ODB_DATA_DIR=/data
EXPOSE 8790
VOLUME ["/data"]
ENTRYPOINT ["odb"]

FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /netflux ./cmd/netflux

# git: for the desired-state checkout and drift branches.
FROM alpine:3.22
RUN apk add --no-cache git ca-certificates && adduser -D -u 10001 netflux
COPY --from=build /netflux /usr/local/bin/netflux
USER netflux
WORKDIR /var/lib/netflux
EXPOSE 8080
ENTRYPOINT ["netflux"]
CMD ["run", "-config", "/etc/netflux/netflux.yaml"]

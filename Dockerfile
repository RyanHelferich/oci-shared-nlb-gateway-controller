FROM golang:1.26.8@sha256:3c3e25a4da13fd0478eed2df1eb35a0e667094a7124d3993a6a1d30f71c17e79 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY api ./api
COPY internalcontroller ./internalcontroller
COPY internalgateway ./internalgateway
COPY internalgatewaypool ./internalgatewaypool
COPY cmd ./cmd
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false -ldflags='-s -w -buildid= -X main.version=0.1.0' -o /controller ./cmd/controller
FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /controller /controller
USER 65532:65532
ENTRYPOINT ["/controller"]

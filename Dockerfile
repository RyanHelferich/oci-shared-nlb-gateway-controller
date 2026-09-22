FROM golang:1.27.1@sha256:3680233e3204827fbdc66088528ae6d4b3d034f51d03a99d454f6de034888244 AS build
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

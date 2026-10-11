FROM golang:1.25.5-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /gateway ./cmd/gateway

FROM alpine:3.22
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 gateway
COPY --from=build /gateway /usr/local/bin/gateway
USER gateway
EXPOSE 8080
ENTRYPOINT ["gateway"]

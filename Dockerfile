FROM golang:1.26-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /bin/auth-hub

FROM gcr.io/distroless/static-debian12

COPY --from=build /bin/auth-hub /auth-hub
EXPOSE 9090
ENTRYPOINT ["/auth-hub"]
CMD ["-config", "/config/config.toml"]

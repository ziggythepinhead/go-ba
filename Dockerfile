FROM docker.io/library/golang:1.26-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# tzdata is embedded via `import _ "time/tzdata"` in cmd/go-ba (Europe/Ljubljana date math),
# so the runtime image needs no zoneinfo package.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/go-ba ./cmd/go-ba

FROM docker.io/library/alpine:3.22
RUN addgroup -S goba && adduser -S goba -G goba
USER goba
WORKDIR /app
COPY --from=build /out/go-ba /app/go-ba
EXPOSE 8080
ENTRYPOINT ["/app/go-ba"]

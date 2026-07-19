FROM docker.io/library/golang:1.26-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# tzdata is embedded via `import _ "time/tzdata"` in cmd/go-be (Europe/Ljubljana date math),
# so the runtime image needs no zoneinfo package.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/go-be ./cmd/go-be

FROM docker.io/library/alpine:3.22
RUN addgroup -S gobe && adduser -S gobe -G gobe
USER gobe
WORKDIR /app
COPY --from=build /out/go-be /app/go-be
EXPOSE 8080
ENTRYPOINT ["/app/go-be"]

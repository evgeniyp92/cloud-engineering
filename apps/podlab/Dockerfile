# Build stage: resolve deps and compile a static binary
FROM golang:1.23 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY main.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /podlab .

# Runtime stage: distroless = no shell, no package manager, tiny attack surface.
# (Day 41 discusses why this matters; the ephemeral-containers lesson on Day 2
# shows how to debug a container that has no shell.)
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /podlab /podlab
EXPOSE 8080
USER nonroot
ENTRYPOINT ["/podlab"]

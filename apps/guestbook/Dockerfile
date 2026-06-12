FROM golang:1.23 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY main.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /guestbook .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /guestbook /guestbook
EXPOSE 8080
USER nonroot
ENTRYPOINT ["/guestbook"]

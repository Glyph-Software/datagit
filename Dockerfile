# Distroless image for the DataGit CLI and server (§17.1).
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/datagit ./cmd/datagit

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/datagit /usr/local/bin/datagit
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/datagit"]

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY main.go .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /reference-agent main.go

FROM scratch
WORKDIR /app
COPY --from=build /reference-agent /app/reference-agent
ENTRYPOINT ["/app/reference-agent"]

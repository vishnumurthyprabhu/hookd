# Built from the repository root (two folders above deploy/aws).
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/hookd ./cmd/hookd

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/hookd /usr/local/bin/hookd
USER nonroot
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/hookd"]
CMD ["serve"]

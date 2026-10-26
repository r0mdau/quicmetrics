FROM golang:1.27-trixie AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/quicmetrics ./cmd/quicmetrics

FROM gcr.io/distroless/static-debian13:nonroot

COPY --from=build /out/quicmetrics /usr/local/bin/quicmetrics
EXPOSE 4242/udp 2223/tcp
ENTRYPOINT ["/usr/local/bin/quicmetrics"]
CMD ["-addr", "0.0.0.0:4242"]

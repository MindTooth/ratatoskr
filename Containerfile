FROM registry.access.redhat.com/ubi10/go-toolset:1.26.7-1791911662@sha256:4240036fd19bdad5d72ad15062d80b71cac9bc6b7789a45e53d6cc3e43167165 AS build

WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -buildvcs=false -tags containers_image_openpgp -trimpath -ldflags='-s -w' -o /tmp/ratatoskr ./cmd/ratatoskr

FROM registry.access.redhat.com/ubi10/ubi-micro:10.2-1791898871@sha256:09d42b63a4ef2a23543502bd09e892c495a4ca480d481e28f00b3070d62d2d24

COPY --from=build /tmp/ratatoskr /usr/local/bin/ratatoskr
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/ratatoskr"]
CMD ["serve", "--config", "/etc/ratatoskr/config.yaml"]

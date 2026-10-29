FROM registry.access.redhat.com/ubi10/go-toolset:1.26.10-1793232609@sha256:5d2ab83e88506afee6bcc909fdab425ab979725ab30d6ad43a02aaa77e2258ff AS build

WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -buildvcs=false -tags containers_image_openpgp -trimpath -ldflags='-s -w' -o /tmp/ratatoskr ./cmd/ratatoskr

FROM registry.access.redhat.com/ubi10/ubi-micro:10.2-1792455530@sha256:1c105e547de5d9e0bc20d06ebabee73b20bc6bba994c50ae3e207684fd8bef2b

COPY --from=build /tmp/ratatoskr /usr/local/bin/ratatoskr
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/ratatoskr"]
CMD ["serve", "--config", "/etc/ratatoskr/config.yaml"]

# syntax=docker/dockerfile:1@sha256:ecfaec9ed6d810b56388c508f4121597bfbba70d41a6dfeee4d8cad5f295fc32

# The buildpack-deps base supplies the C toolchain as well as Go, so no mutable
# package repository is consulted during either build.
ARG BUILD_IMAGE=golang:1.26.6-bookworm@sha256:116d58cbd88c1297624acc6e967a060012422bacf9930927e23fb719189c6f36

FROM ${BUILD_IMAGE} AS temperature-build
WORKDIR /build
ADD --checksum=sha256:89299633cc78c17032547aa78b9df1d9bf08c5bd6279cde7750bd4d4367afb81 \
    https://github.com/Mellanox/mstflint/releases/download/v4.37.0-1/mstflint-4.37.0-1.tar.gz \
    /build/mstflint.tar.gz
RUN tar -xzf mstflint.tar.gz
WORKDIR /build/mstflint-4.37.0
RUN ./configure --disable-dc --disable-openssl --disable-inband \
      --disable-fw-mgr --disable-adb-generic-tools --disable-nvml \
      --enable-all-static \
    && make -j4 -C tools_layouts \
    && make -j4 -C common \
    && make -j4 -C mtcr_ul \
    && make -j4 -C reg_access \
    && make -j4 -C dev_mgt libdev_mgt.la \
    && make -j4 -C small_utils mstmget_temp mstmget_temp_LDFLAGS=-all-static \
    && small_utils/mstmget_temp --version \
    && small_utils/mstmget_temp --help \
    && ! readelf -l small_utils/mstmget_temp | grep -q INTERP
RUN mkdir -p /out/usr/local/bin /out/usr/share/licenses/mstflint /out/tmp \
    && chmod 0755 /out/tmp \
    && install -m 0755 small_utils/mstmget_temp /out/usr/local/bin/mstmget_temp \
    && strip /out/usr/local/bin/mstmget_temp \
    && install -m 0644 COPYING LICENSE /out/usr/share/licenses/mstflint/ \
    && install -m 0644 debian/copyright /out/usr/share/licenses/mstflint/copyright

FROM ${BUILD_IMAGE} AS exporter-build
WORKDIR /src
ENV CGO_ENABLED=0 GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off
COPY go.mod *.go ./
RUN go build -trimpath -ldflags='-s -w' -o /out/mellanox-temperature-exporter .

FROM exporter-build AS test
RUN CGO_ENABLED=1 go test -race -count=1 ./... && go vet ./...

# Ordinary OCI image. A host administrator grants the required PCI access at
# runtime; neither build stage accesses hardware.
FROM scratch AS exporter
LABEL org.opencontainers.image.title="talos-mellanox-temperature-exporter" \
      org.opencontainers.image.source="https://github.com/KeenWill/talos-mellanox-temperature-exporter" \
      org.opencontainers.image.description="Continuously polled Mellanox NIC ASIC temperatures for Prometheus"
COPY --from=temperature-build /out/ /
COPY --from=exporter-build /out/mellanox-temperature-exporter /usr/local/bin/mellanox-temperature-exporter
COPY LICENSE /usr/share/licenses/mellanox-temperature-exporter/LICENSE
USER 0:0
EXPOSE 9835
ENTRYPOINT ["/usr/local/bin/mellanox-temperature-exporter"]

# Talos mounts this service rootfs and runs its entrypoint from the service YAML.
FROM scratch AS talos-extension
LABEL org.opencontainers.image.title="talos-mellanox-temperature-exporter extension" \
      org.opencontainers.image.source="https://github.com/KeenWill/talos-mellanox-temperature-exporter" \
      org.opencontainers.image.description="Talos system extension for Mellanox NIC ASIC temperature collection"
COPY extension/manifest.yaml /manifest.yaml
COPY extension/nic-temperature.yaml /rootfs/usr/local/etc/containers/nic-temperature.yaml
COPY --from=exporter / /rootfs/usr/local/lib/containers/nic-temperature/

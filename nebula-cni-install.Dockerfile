FROM golang:1 AS build

ENV CGO_ENABLED=0

WORKDIR /work
COPY . ./

RUN make nebula-cni-plugin nebula-cni-instance nebula-cni-install

FROM scratch
COPY --from=build /work/nebula-cni-plugin /
COPY --from=build /work/nebula-cni-instance /
COPY --from=build /work/nebula-cni-install /

ENTRYPOINT ["/nebula-cni-install"]

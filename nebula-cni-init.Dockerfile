FROM golang:1 AS build

ENV CGO_ENABLED=0

WORKDIR /work
COPY . ./

RUN make nebula-cni-init

FROM gcr.io/distroless/static:nonroot
COPY --from=build /work/nebula-cni-init /

ENTRYPOINT ["/nebula-cni-init"]

# One image per service, built from one file: pass CMD to pick the binary.
#
#   docker build --build-arg CMD=tally-reporting -t tally-reporting:dev .
#
# The result is a static binary on distroless, so dev and prod run the same
# image (roadmap/00-conventions.md section 1).

FROM golang:1.27.1-alpine AS build

ARG CMD
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
# Fail early and clearly rather than letting ./cmd/ resolve to the directory.
RUN test -n "${CMD}" || (echo "build argument CMD is required, e.g. --build-arg CMD=tally-reporting" >&2; exit 1)
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/service "./cmd/${CMD}"

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/service /service
USER nonroot:nonroot
ENTRYPOINT ["/service"]

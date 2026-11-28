# AgentGate image: one static binary on distroless (nonroot, no shell).
# The same image runs the gateway (`agentgate serve`) and the paid tool
# (`agentgate paidtool`); the command is chosen by the Helm chart.
#
#   docker build --build-arg VERSION=$(git rev-parse --short HEAD) -t agentgate:dev .
#
# Base images are pinned by digest (multi-arch index). To move them, pull the
# new tag and copy the index digest from `docker buildx imagetools inspect`.

FROM golang:1.26-alpine@sha256:ecb67f987aa1a57794c91d19ade7ae0209f2f0f5496aaef9e6ae85ddf92d02ab AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY console ./console
COPY internal ./internal
COPY migrations ./migrations
ARG VERSION=dev
# Migrations, the console's web files and the mock USDC artifacts are embedded.
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/agentgate ./cmd/agentgate

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
ARG VERSION=dev
LABEL org.opencontainers.image.source="https://github.com/ws4charlie/agent-gate" \
      org.opencontainers.image.version="${VERSION}"
COPY --from=build /out/agentgate /agentgate
USER 65532:65532
ENTRYPOINT ["/agentgate"]
CMD ["serve"]

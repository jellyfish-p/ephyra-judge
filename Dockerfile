# First-release Linux isolation is unvalidated. Docker may need extra
# privileges and/or host user namespaces (for example --privileged, or
# SYS_ADMIN plus host userns) before go-sandbox can isolate untrusted
# programs. Do not treat this image as a validated judge host.

FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -o /out/ephyra-judge ./cmd/ephyra-judge

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
		gcc \
		g++ \
		ca-certificates \
	&& rm -rf /var/lib/apt/lists/*
COPY --from=build /out/ephyra-judge /usr/local/bin/ephyra-judge
ENTRYPOINT ["ephyra-judge"]

# Isolation is implemented (namespaces, cgroup, seccomp) but must still be
# accepted on a recorded Linux host before running untrusted programs.
# Docker may need extra privileges (for example --privileged, or SYS_ADMIN
# plus host userns) before go-sandbox can create containers.

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
ADD https://github.com/MikeMirzayanov/testlib/releases/download/0.9.41/testlib.h /usr/include/testlib.h
COPY --from=build /out/ephyra-judge /usr/local/bin/ephyra-judge
ENV EPHYRA_JUDGE_TESTLIB=/usr/include/testlib.h
ENTRYPOINT ["ephyra-judge"]

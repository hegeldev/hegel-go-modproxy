FROM golang:1.26.5-alpine@sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2 AS base

RUN apk add --no-cache git git-lfs \
	&& git lfs install --system

FROM base AS builder

WORKDIR /app

COPY go.* .

RUN go mod download

COPY . .

# Static binary so it runs without libc in the runtime image.
RUN CGO_ENABLED=0 go build -o /lfsproxy

# Test stage: has go + git + git-lfs together so the integration suite can run
# fully offline (`docker run --network=none ...`). Not part of the default
# build target (runtime, below); select it with `--target test`.
FROM builder AS test

CMD ["go", "test", "-v", "./..."]

FROM base

COPY --from=builder /lfsproxy /lfsproxy

ENV PORT=10000

EXPOSE ${PORT}

ENTRYPOINT ["/lfsproxy"]

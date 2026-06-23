FROM golang:1.26.4-alpine@sha256:3ad57304ad93bbec8548a0437ad9e06a455660655d9af011d58b993f6f615648 AS base

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

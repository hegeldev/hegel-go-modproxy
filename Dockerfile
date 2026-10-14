FROM golang:1.27.2-alpine@sha256:f92b6ef800e499660581efdabdf25d9d817a9d124eaf900924f0504e7e27e12d AS base

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

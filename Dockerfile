FROM golang:1.26-alpine AS builder

WORKDIR /app

COPY go.* .

RUN go mod download

COPY *.go .

# Static binary so it runs without libc in the runtime image.
RUN CGO_ENABLED=0 go build -o /lfsproxy

FROM golang:1.26-alpine

# git and git-lfs are required at runtime: GoFetcher shells out to `go`, which
# clones modules over git, and git-lfs materializes large files on checkout.
RUN apk add --no-cache git git-lfs \
	&& git lfs install --system

COPY --from=builder /lfsproxy /lfsproxy

ENV PORT=10000

EXPOSE ${PORT}

ENTRYPOINT ["/lfsproxy"]

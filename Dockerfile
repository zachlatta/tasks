# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags "-s -w" -o /out/task-tracker ./cmd/task-tracker

FROM alpine:3.20
RUN apk add --no-cache ca-certificates wget && adduser -D -u 10001 app
COPY --from=build /out/task-tracker /usr/local/bin/task-tracker
# Only used for local image uploads; tasks live in PostgreSQL. Mount a volume
# here (or use S3) when TASK_TRACKER_OBJECT_STORE=local.
RUN mkdir -p /data && chown app:app /data
USER app
ENV TASK_TRACKER_ADDR=0.0.0.0:8080 \
    TASK_TRACKER_DATA_DIR=/data
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["task-tracker"]
CMD ["serve"]

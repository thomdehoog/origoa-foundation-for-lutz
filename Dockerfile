FROM golang:1.26-alpine AS build
WORKDIR /app
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /origoa ./cmd/origoa

FROM alpine:3.23
RUN apk add --no-cache git && addgroup -S origoa && adduser -S -G origoa origoa
COPY --from=build /origoa /usr/local/bin/origoa
RUN mkdir /data && chown origoa:origoa /data
ENV ORIGOA_HOST=0.0.0.0 \
    ORIGOA_REPOSITORY=/data
VOLUME ["/data"]
EXPOSE 3000
USER origoa
CMD ["origoa"]

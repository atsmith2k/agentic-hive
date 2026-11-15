# The panel builds with build.sh directly; this image is for anywhere else.
FROM golang:1.26-alpine AS build
RUN apk add --no-cache curl
WORKDIR /src
COPY . .
RUN sh build.sh

FROM alpine:3.22
RUN adduser -D -u 10001 hive && mkdir /data && chown hive /data
USER hive
COPY --from=build /src/bin/hive /usr/local/bin/hive
ENV HOST=0.0.0.0 PORT=8080 DB_PATH=/data/hive.db
VOLUME /data
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s CMD wget -qO- http://127.0.0.1:8080/healthz | grep -q ok
CMD ["hive"]

# Node image: the static tailraid binary on a minimal base with a shell for entry.sh.
FROM alpine:3.20
RUN apk add --no-cache ca-certificates curl
COPY tailraid-linux-arm64 /usr/local/bin/tailraid
COPY entry.sh /entry.sh
RUN chmod +x /entry.sh /usr/local/bin/tailraid
ENV TAILRAID_HOME=/var/lib/tailraid
ENTRYPOINT ["/entry.sh"]

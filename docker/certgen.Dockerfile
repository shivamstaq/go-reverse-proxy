# syntax=docker/dockerfile:1
#
# One-shot job: generates the CA and the two leaf certificates into /certs.
FROM alpine:3.22
RUN apk add --no-cache openssl

COPY scripts/certgen.sh /usr/local/bin/certgen.sh
RUN chmod +x /usr/local/bin/certgen.sh

# Runs as root so it can write to a fresh volume, then hands ownership to the
# unprivileged uid the proxy and upstream run as.
ENV CERT_OWNER=10001:10001

ENTRYPOINT ["/usr/local/bin/certgen.sh"]
CMD ["/certs"]

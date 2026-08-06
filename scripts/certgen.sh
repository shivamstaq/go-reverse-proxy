#!/usr/bin/env sh
set -eu

OUT="${1:-./certs}"
DAYS=365

mkdir -p "$OUT"

# In a container the job runs as root so it can write to a fresh volume; the
# services read the files as an unprivileged uid. Unset locally, this is a no-op.
take_ownership() {
  if [ -n "${CERT_OWNER:-}" ]; then
    chown -R "$CERT_OWNER" "$OUT"
  fi
}

# Reissuing on every run would break partial restarts: services read their
# certificates once at startup, and Compose does not recreate a service just
# because this job ran again — it would then present a leaf from the old CA and
# every request would fail verification. So this is idempotent. Use
# `docker compose down -v`, or REGENERATE=1, to force a new CA.
if [ -z "${REGENERATE:-}" ] &&
	[ -f "$OUT/ca.crt" ] && [ -f "$OUT/proxy.crt" ] && [ -f "$OUT/upstream.crt" ]; then
	take_ownership
	echo "certificates already present in $OUT; set REGENERATE=1 to reissue"
	exit 0
fi

rm -f "$OUT"/*.crt "$OUT"/*.key "$OUT"/*.srl

# ---- CA ----
openssl ecparam -name prime256v1 -genkey -noout -out "$OUT/ca.key"
openssl req -x509 -new -key "$OUT/ca.key" \
  -sha256 -days "$DAYS" \
  -subj "/CN=test-keyword-local-ca" \
  -addext "basicConstraints=critical,CA:TRUE" \
  -addext "keyUsage=critical,keyCertSign,cRLSign" \
  -out "$OUT/ca.crt"

# ---- leaf certs ----
gen_leaf() {
  name="$1"
  sans="$2"

  openssl ecparam -name prime256v1 -genkey -noout -out "$OUT/$name.key"
  openssl req -new -key "$OUT/$name.key" -subj "/CN=$name" -out "$OUT/$name.csr"

  cat > "$OUT/$name.ext" <<EOF
basicConstraints=CA:FALSE
keyUsage=critical,digitalSignature
extendedKeyUsage=serverAuth
subjectAltName=$sans
EOF

  openssl x509 -req -in "$OUT/$name.csr" \
    -CA "$OUT/ca.crt" -CAkey "$OUT/ca.key" -CAcreateserial \
    -sha256 -days "$DAYS" \
    -extfile "$OUT/$name.ext" \
    -out "$OUT/$name.crt"

  rm -f "$OUT/$name.csr" "$OUT/$name.ext"
  chmod 600 "$OUT/$name.key"
  chmod 644 "$OUT/$name.crt"
}

gen_leaf proxy    "DNS:proxy,DNS:localhost,IP:127.0.0.1,IP:::1"
gen_leaf upstream "DNS:upstream,DNS:localhost,IP:127.0.0.1,IP:::1"

chmod 600 "$OUT/ca.key"
chmod 644 "$OUT/ca.crt"

take_ownership

echo "certificates written to $OUT"

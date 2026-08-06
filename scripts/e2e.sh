#!/usr/bin/env bash
#
# End-to-end check of the running Compose stack against every documented rule.
#
#   docker compose up -d --build
#   ./scripts/e2e.sh
#
# Talks to the proxy over real HTTPS using the CA from the shared volume, so it
# verifies the deployment rather than a test double. Keyword and replacement are
# read from proxy.env, and matched with grep -F: a replacement like [REDACTED]
# would otherwise be treated as a regex character class.

set -uo pipefail

BASE=${BASE:-https://localhost:8443}
ENV_FILE=${ENV_FILE:-proxy.env}

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

pass=0
fail=0
ok()    { printf '  \033[32mPASS\033[0m %s\n' "$1"; pass=$((pass + 1)); }
bad()   { printf '  \033[31mFAIL\033[0m %s — %s\n' "$1" "$2"; fail=$((fail + 1)); }
check() { [ "$2" = "$3" ] && ok "$1" || bad "$1" "got [$2] want [$3]"; }

[ -f "$ENV_FILE" ] || { echo "run from the repository root: $ENV_FILE not found" >&2; exit 2; }
KEY=$(sed -n 's/^KEYWORD=//p' "$ENV_FILE")
REP=$(sed -n 's/^REPLACEMENT=//p' "$ENV_FILE")

CA=$WORK/ca.crt
docker compose cp proxy:/certs/ca.crt "$CA" >/dev/null 2>&1 ||
	{ echo "could not copy the CA out of the proxy container; is the stack up?" >&2; exit 2; }
C=(curl -sS --cacert "$CA")

# The stack has no healthcheck, so wait for the whole chain rather than a port.
for _ in $(seq 30); do
	[ "$("${C[@]}" -o /dev/null -w '%{http_code}' "$BASE/json" 2>/dev/null)" = "200" ] && break
	sleep 1
done

echo "config: KEYWORD=$KEY REPLACEMENT=$REP"
echo

echo "TLS on both hops"
check "client speaks HTTP/2 over TLS" "$("${C[@]}" -o /dev/null -w '%{http_version}' "$BASE/json")" "2"
curl -sS -o /dev/null "$BASE/json" 2>/dev/null &&
	bad "client without the CA is refused" "the request succeeded" || ok "a client without the CA is refused"
curl -sS -o /dev/null --cacert "$CA" --tlsv1.1 --tls-max 1.1 "$BASE/json" 2>/dev/null &&
	bad "obsolete TLS is refused" "TLS 1.1 was accepted" || ok "TLS 1.1 is refused"
docker compose exec -T proxy sh -c 'wget -q -O- http://upstream:8443/json' >/dev/null 2>&1 &&
	bad "upstream is TLS-only" "plaintext HTTP worked" || ok "the upstream refuses plaintext"
case "$(docker compose ps --format '{{.Service}} {{.Ports}}' | grep upstream)" in
	*0.0.0.0*) bad "upstream is not published" "it is reachable from the host" ;;
	*) ok "only the proxy publishes a port" ;;
esac

echo
echo "Textual responses are rewritten"
for p in /text /html /json /xml /js; do
	body=$("${C[@]}" "$BASE$p")
	if printf '%s' "$body" | grep -qF "$KEY"; then bad "$p rewritten" "the keyword survived"
	elif printf '%s' "$body" | grep -qF "$REP"; then ok "$p rewritten"
	else bad "$p rewritten" "no replacement found"; fi
done
edges=$("${C[@]}" "$BASE/text")
check "/text has 3 replacements" "$(printf '%s' "$edges" | grep -oF "$REP" | wc -l)" "3"
case "$edges" in "$REP"*) ok "a keyword at the start is replaced" ;; *) bad "keyword at start" "$edges" ;; esac
case "$edges" in *"$REP") ok "a keyword at the end is replaced" ;; *) bad "keyword at end" "$edges" ;; esac
check "an error body is rewritten too" "$("${C[@]}" "$BASE/status/404" | grep -cF "$REP")" "1"

echo
echo "Binary passes through untouched"
"${C[@]}" "$BASE/image" -o "$WORK/image.png"
origin=$(docker compose exec -T upstream sh -c \
	'wget -q --no-check-certificate -O- https://127.0.0.1:8443/image | sha256sum' | cut -d' ' -f1)
check "the PNG is byte-identical to the origin" "$(sha256sum "$WORK/image.png" | cut -d' ' -f1)" "$origin"
grep -qaF "$KEY" "$WORK/image.png" && ok "the keyword inside the PNG is untouched" || bad "PNG keyword" "it was rewritten"

echo
echo "Gzip is decoded, rewritten and re-encoded"
"${C[@]}" -H 'Accept-Encoding: gzip' -D "$WORK/gzip.head" -o "$WORK/gzip.bin" "$BASE/gzip"
check "still Content-Encoding: gzip" \
	"$(awk 'tolower($1)=="content-encoding:"{print $2}' "$WORK/gzip.head" | tr -d '\r')" "gzip"
check "Content-Length matches the bytes sent" \
	"$(awk 'tolower($1)=="content-length:"{print $2}' "$WORK/gzip.head" | tr -d '\r')" "$(wc -c < "$WORK/gzip.bin")"
grep -qF "$REP" "$WORK/gzip.bin" && bad "the wire bytes are compressed" "plaintext is visible" || ok "the wire bytes are compressed"
plain=$(gunzip -c "$WORK/gzip.bin")
printf '%s' "$plain" | grep -qF "$REP" && ok "it decompresses to rewritten HTML" || bad "gzip content" "$plain"
printf '%s' "$plain" | grep -qF "$KEY" && bad "gzip content" "the keyword survived" || ok "no keyword after decompression"

echo
echo "Encodings and streams the proxy cannot rewrite are forwarded"
check "brotli is untouched" "$("${C[@]}" "$BASE/brotli")" "not-really-brotli $KEY"
# Cutting an open stream short is the point here, so curl's timeout is expected.
check "an event stream keeps its type" \
	"$("${C[@]}" -o /dev/null -D - --max-time 1 "$BASE/events" 2>/dev/null | awk 'tolower($1)=="content-type:"{print $2}' | tr -d '\r')" \
	"text/event-stream"
# The fixture waits 1.5s after its first event, so anything arriving inside 1s
# proves the stream was not buffered for rewriting.
first=$(timeout 1 curl -sN --cacert "$CA" "$BASE/events" | head -1)
printf '%s' "$first" | grep -qF "$KEY" &&
	ok "the first event arrives immediately, unrewritten" || bad "SSE streaming" "got [$first]"

echo
echo "The outgoing Host header"
echoed=$("${C[@]}" "$BASE/echo")
check "the upstream sees its own authority" \
	"$(printf '%s' "$echoed" | python3 -c 'import json,sys; print(json.load(sys.stdin)["host"])')" "upstream:8443"
check "the client Host is kept in X-Forwarded-Host" \
	"$(printf '%s' "$echoed" | python3 -c 'import json,sys; print(json.load(sys.stdin)["headers"]["X-Forwarded-Host"][0])')" \
	"localhost:8443"
check "an existing forwarded chain survives" \
	"$("${C[@]}" -H 'X-Forwarded-Host: edge.example.com' "$BASE/echo" |
		python3 -c 'import json,sys; print(json.load(sys.stdin)["headers"]["X-Forwarded-Host"][0])')" \
	"edge.example.com"

echo
echo "Transfer, status, headers and methods"
check "a chunked response is rewritten" "$("${C[@]}" "$BASE/chunked" | grep -cF "$REP")" "3"
for code in 201 404 500; do
	check "status $code is preserved" "$("${C[@]}" -o /dev/null -w '%{http_code}' "$BASE/status/$code")" "$code"
done
for code in 204 304; do
	got=$("${C[@]}" -o "$WORK/body.bin" -w '%{http_code}' "$BASE/status/$code")
	[ "$got" = "$code" ] && [ ! -s "$WORK/body.bin" ] &&
		ok "status $code stays bodyless" || bad "status $code" "code=$got size=$(wc -c < "$WORK/body.bin")"
done
check "a redirect is not followed" "$("${C[@]}" -o /dev/null -w '%{http_code}' "$BASE/redirect")" "302"
check "Location is preserved" \
	"$("${C[@]}" -D - -o /dev/null "$BASE/redirect" | awk 'tolower($1)=="location:"{print $2}' | tr -d '\r')" "/html"
check "a custom response header is preserved" "$("${C[@]}" -D - -o /dev/null "$BASE/headers" | grep -ic '^x-custom-header')" "1"
check "Set-Cookie is preserved" "$("${C[@]}" -D - -o /dev/null "$BASE/headers" | grep -ic '^set-cookie')" "1"
check "HEAD returns no body" "$("${C[@]}" -I -o /dev/null -w '%{size_download}' "$BASE/html")" "0"
check "HEAD drops a length the GET would contradict" "$("${C[@]}" -I "$BASE/html" | grep -ic '^content-length')" "0"
for m in POST PUT DELETE PATCH OPTIONS; do
	check "$m is forwarded" "$("${C[@]}" -o /dev/null -w '%{http_code}' -X $m -d x "$BASE/echo")" "200"
done
check "the request body reaches the upstream" \
	"$("${C[@]}" -X POST -d sentinel-payload "$BASE/echo" |
		python3 -c 'import json,sys; print(json.load(sys.stdin)["body"])')" "sentinel-payload"
check "CONNECT is refused rather than relayed" "$("${C[@]}" -o /dev/null -w '%{http_code}' -X CONNECT "$BASE/")" "501"

echo
echo "No size cap: large bodies are rewritten too"
"${C[@]}" "$BASE/large?mb=8" -o "$WORK/large.bin"
grep -qaF "$KEY" "$WORK/large.bin" && bad "8 MiB is rewritten" "the keyword survived" || ok "an 8 MiB body is rewritten"
grep -qaF "$REP" "$WORK/large.bin" && ok "it contains the replacement" || bad "8 MiB replacement" "not found"

# Completeness is derived from the origin, not assumed: a replacement shorter or
# longer than the keyword legitimately changes the body's length, so the expected
# size is the origin's minus what the substitution removed. Comparing occurrence
# counts as well is what would catch a truncated body, since a short read would
# lose replacements too.
docker compose exec -T upstream sh -c \
	'wget -q --no-check-certificate -O- "https://127.0.0.1:8443/large?mb=8"' > "$WORK/large.raw"
raw_size=$(wc -c < "$WORK/large.raw")
raw_hits=$(grep -oaF "$KEY" "$WORK/large.raw" | wc -l)
key_len=$(printf '%s' "$KEY" | wc -c)
rep_len=$(printf '%s' "$REP" | wc -c)

check "every occurrence was replaced" "$(grep -oaF "$REP" "$WORK/large.bin" | wc -l)" "$raw_hits"
check "it arrives complete, allowing for the length the rewrite changed" \
	"$(wc -c < "$WORK/large.bin")" "$((raw_size - raw_hits * (key_len - rep_len)))"

echo
echo "Logging and lifecycle"
"${C[@]}" -o /dev/null "$BASE/html"
logs=$(docker compose logs proxy --no-log-prefix --tail=40)
printf '%s' "$logs" | grep -q '"request_id":""' && bad "request_id is populated" "it is empty" || ok "request_id is populated"
printf '%s' "$logs" | grep -q '"host":"localhost:8443"' &&
	ok "the access log records the client Host" || bad "access log Host" "the client Host is missing"
printf '%s' "$logs" | grep -q '"host":"upstream:8443"' &&
	bad "access log Host" "it reports the upstream" || ok "the access log does not report the upstream Host"
check "every log line is JSON" "$(printf '%s' "$logs" | grep -vc '^{')" "0"
start=$(date +%s)
docker compose stop proxy >/dev/null 2>&1
elapsed=$(($(date +%s) - start))
[ "$elapsed" -le 11 ] && ok "graceful shutdown in ${elapsed}s" || bad "shutdown" "it took ${elapsed}s"
docker compose start proxy >/dev/null 2>&1
sleep 6
check "restarting only the proxy still works" "$("${C[@]}" -o /dev/null -w '%{http_code}' "$BASE/json")" "200"

echo
printf 'passed %d, failed %d\n' "$pass" "$fail"
[ "$fail" -eq 0 ]

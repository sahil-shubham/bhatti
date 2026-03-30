#!/bin/bash
set -euo pipefail

ROUTES_FILE="/opt/bhatti-caddy/routes.yml"
OUTPUT_FILE="/opt/bhatti-caddy/sites.caddy"
DOMAIN="dev2.ctxmill.com"
PUBLIC_IP="100.72.221.54"
BHATTI_URL="http://localhost:8080"

BHATTI_TOKEN="${BHATTI_TOKEN:-$(grep auth_token /root/.bhatti/config.yaml 2>/dev/null | awk '{print $2}' || echo '')}"
CF_TOKEN="${CF_DNS_API_TOKEN:-}"
CF_ZONE_ID="ccd005bbeca3ab5e58af32b4f7308273"

if [[ -z "$BHATTI_TOKEN" ]]; then
    echo "error: BHATTI_TOKEN not set" >&2
    exit 1
fi

ensure_dns_record() {
    local name="$1"
    [[ -z "$CF_TOKEN" ]] && return 0
    local existing
    existing=$(curl -sf -H "Authorization: Bearer $CF_TOKEN" -H "Content-Type: application/json" \
        "https://api.cloudflare.com/client/v4/zones/$CF_ZONE_ID/dns_records?type=A&name=$name" \
        | jq -r '.result[0].id // empty') || true
    if [[ -n "$existing" ]]; then
        curl -sf -H "Authorization: Bearer $CF_TOKEN" -H "Content-Type: application/json" \
            -X PUT "https://api.cloudflare.com/client/v4/zones/$CF_ZONE_ID/dns_records/$existing" \
            -d "{\"type\":\"A\",\"name\":\"$name\",\"content\":\"$PUBLIC_IP\",\"ttl\":300,\"proxied\":false}" > /dev/null || true
    else
        curl -sf -H "Authorization: Bearer $CF_TOKEN" -H "Content-Type: application/json" \
            -X POST "https://api.cloudflare.com/client/v4/zones/$CF_ZONE_ID/dns_records" \
            -d "{\"type\":\"A\",\"name\":\"$name\",\"content\":\"$PUBLIC_IP\",\"ttl\":300,\"proxied\":false}" > /dev/null || true
    fi
}

get_sandbox_info() {
    local name="$1"
    curl -sf -H "Authorization: Bearer $BHATTI_TOKEN" "$BHATTI_URL/sandboxes" \
        | jq -r ".[] | select(.name == \"$name\") | \"\(.id) \(.ip)\""
}


declare -A SUBDOMAINS SANDBOXES PORTS
route_count=0
while IFS=: read -r subdomain rest; do
    subdomain=$(echo "$subdomain" | xargs)
    rest=$(echo "$rest" | xargs)
    [[ -z "$subdomain" || "$subdomain" == \#* ]] && continue
    if [[ "$rest" == *:* ]]; then
        sandbox="${rest%%:*}"
        port="${rest##*:}"
    else
        sandbox="$subdomain"
        port="$rest"
    fi
    sandbox=$(echo "$sandbox" | xargs)
    port=$(echo "$port" | xargs)
    SUBDOMAINS[$route_count]="$subdomain"
    SANDBOXES[$route_count]="$sandbox"
    PORTS[$route_count]="$port"
    route_count=$((route_count + 1))
done < "$ROUTES_FILE"

if [[ $route_count -eq 0 ]]; then
    echo "# No routes" > "$OUTPUT_FILE"
    echo "No routes in $ROUTES_FILE"
    exit 0
fi

{
    for ((i=0; i<route_count; i++)); do
        subdomain="${SUBDOMAINS[$i]}"
        sandbox="${SANDBOXES[$i]}"
        port="${PORTS[$i]}"
        info=$(get_sandbox_info "$sandbox")

        if [[ -z "$info" ]]; then
            echo "  SKIP: sandbox '$sandbox' not found (subdomain: $subdomain)" >&2
            continue
        fi

        sandbox_id=$(echo "$info" | awk '{print $1}')
        sandbox_ip=$(echo "$info" | awk '{print $2}')

        echo "Route: $subdomain.$DOMAIN -> $sandbox_ip:$port ($sandbox)" >&2

        ensure_dns_record "$subdomain.$DOMAIN"
        ensure_dns_record "*.$subdomain.$DOMAIN"

        cat << SITE
${subdomain}.${DOMAIN}, *.${subdomain}.${DOMAIN} {
	forward_auth ${BHATTI_URL} {
		uri /sandboxes/${sandbox_id}
		header_up Authorization "Bearer ${BHATTI_TOKEN}"
	}
	reverse_proxy ${sandbox_ip}:${port}
}

SITE
    done
} > "${OUTPUT_FILE}.tmp"

mv "${OUTPUT_FILE}.tmp" "$OUTPUT_FILE"
echo "Written $OUTPUT_FILE ($route_count routes)" >&2

caddy reload --config /opt/bhatti-caddy/Caddyfile 2>/dev/null || true

# Run the gateway

The gateway serves stagelet traffic. The control plane sends it a route for each stagelet: a host name and the address of the stagelet's public port. The gateway admits a visitor only after a handoff from Stagely, and it holds no cloud credentials or application secrets.

## Start the gateway

With certificates from Let's Encrypt:

```sh
stagely-gateway run -control-plane wss://<control plane>/v1/connect -enrollment-token <gateway token> \
  -acme -acme-email ops@example.com -listen :443
```

With a certificate from files:

```sh
stagely-gateway run -control-plane wss://<control plane>/v1/connect -enrollment-token <gateway token> \
  -tls-cert /etc/stagely/tls.crt -tls-key /etc/stagely/tls.key -listen :443
```

The gateway needs its enrollment token only on the first start. After that, the key in `<data-dir>/gateway.key` identifies it.

| Flag | Default | Meaning |
| --- | --- | --- |
| `-control-plane` | `ws://localhost:8080/v1/connect` | Control plane session URL |
| `-data-dir` | `~/.stagely/gateway` | Directory for the gateway key and, with `-acme`, the certificate cache |
| `-enrollment-token` | none | One-time enrollment token, needed on the first start |
| `-listen` | `:8443` | HTTPS listen address |
| `-tls-cert`, `-tls-key` | none | Certificate and key files for the stagelet host names. Required unless `-acme` is set. |
| `-acme` | off | Get a certificate for each stagelet host name through ACME. Do not set `-tls-cert` or `-tls-key` with it. |
| `-acme-directory` | `https://acme-v02.api.letsencrypt.org/directory` | ACME directory URL. The default is Let's Encrypt production. |
| `-acme-email` | none | Contact email for the ACME account |
| `-acme-directory-ca` | none | PEM file with CA certificates to trust for the ACME server's own HTTPS, in addition to the system roots. Test servers such as Pebble need it. |
| `-acme-extra-hosts` | none | Comma-separated host names that get certificates in addition to the stagelet host names |
| `-http-listen` | `:80` | With `-acme`: HTTP listen address. It answers HTTP-01 challenges and redirects all other requests to HTTPS. |

## Ports

| Port | Direction | Use |
| --- | --- | --- |
| 443 | Inbound from the internet | Stagelet traffic over HTTPS. With `-acme`, also the TLS-ALPN-01 challenge. |
| 80 | Inbound from the internet | With `-acme` only: the HTTP-01 challenge and redirects to HTTPS |
| Control plane port | Outbound | The WebSocket session to the control plane |
| ACME server port, usually 443 | Outbound | With `-acme` only: account, orders and certificates |
| Stagelet ports | Outbound to the stagelet machines | Proxied traffic and route health checks |

The ACME server checks challenges on ports 443 and 80 exactly. If the gateway listens on other ports, forward 443 and 80 to them. Ports below 1024 need root or the `CAP_NET_BIND_SERVICE` capability on Linux.

## Certificates from files

Use one wildcard certificate for `*.<stagelet domain>`. Get it outside the gateway: a wildcard certificate needs the DNS-01 challenge, and that needs DNS credentials, which the gateway must not hold. Restart the gateway after you replace the files.

## Certificates through ACME

With `-acme`, the gateway gets one certificate for each stagelet host name. It needs no cloud or DNS credentials.

Before you start:

- Point a wildcard DNS record, `*.<stagelet domain>`, at the gateway's public address.
- Open ports 443 and 80 to the internet.
- Put `-data-dir` on a disk that survives a restart or a new machine image.

How it works:

1. A visitor opens a stagelet host for the first time. The TLS handshake asks the gateway for a certificate.
2. The gateway checks that the host name is in its current route table or in `-acme-extra-hosts`. It refuses every other name, so nobody can make it request certificates for names it does not serve.
3. The gateway gets a certificate through the TLS-ALPN-01 challenge on port 443. If that fails, it tries the HTTP-01 challenge on port 80. This first handshake waits a few seconds.
4. The gateway keeps the certificate and the ACME account key in `<data-dir>/acme`. Only the owner can read that directory. After a restart, the gateway uses the stored certificates.
5. The gateway renews each certificate before it expires.

When you set `-acme`, the gateway accepts the terms of service of the ACME server.

Things to know:

- Every certificate goes into public Certificate Transparency logs, so each stagelet host name becomes public. Access still needs a handoff from Stagely.
- The gateway does not delete certificates when a stagelet goes away. It keeps renewing the certificates it loaded since it started, also for deleted stagelets, until it restarts. You can delete old files from `<data-dir>/acme`.
- After a failed attempt for a host name, the gateway tries again one minute later, on the next connection to that host.
- A client that does not support ECDSA certificates gets a second, RSA certificate for the same host name. Current browsers support ECDSA.
- The gateway does not support ACME servers that require External Account Binding.

## Let's Encrypt rate limits

Each new stagelet gets a new certificate, so the Let's Encrypt limits put a ceiling on how fast you can create stagelets. A new deploy to an existing stagelet uses the same host name and the stored certificate, so it does not count.

| Limit | Value | What it means for stagelets |
| --- | --- | --- |
| New certificates per registered domain | 50 in 7 days, then 1 more every 202 minutes | At most about 50 new stagelets a week under one registered domain, such as `example.com`. Every other certificate under that domain counts too. |
| New certificates per exact set of names | 5 in 7 days | Each loss of `<data-dir>/acme` gets every stagelet a new certificate for the same name. After four losses in 7 days, the gateway cannot get certificates for those stagelets. |
| New orders per account | 300 in 3 hours | Above this rate, new stagelets get no certificate until the limit refills. |
| Authorization failures per host name and account | 5 an hour | A wrong DNS record or a closed port blocks that host name for a while. |
| New accounts per IP address | 10 in 3 hours | Each new, empty cache directory creates a new account. |

Renewals of the same host name do not count against the first and the third limits. The full rules are at <https://letsencrypt.org/docs/rate-limits/>.

To stay within the limits:

- Count the new stagelets you expect each week. One pull request usually means one stagelet.
- Put stagelets under a registered domain that you use for nothing else.
- Keep `<data-dir>/acme` on persistent disk and back it up with the gateway key.
- Try new setups against the Let's Encrypt staging directory first: `-acme-directory https://acme-staging-v02.api.letsencrypt.org/directory`. Browsers do not trust its certificates.
- If you need more than about 50 new stagelets a week, use a wildcard certificate from files, ask Let's Encrypt for a higher limit, or use another ACME server with `-acme-directory`. A request for a higher limit takes a few weeks.

## Test ACME locally with Pebble

[Pebble](https://github.com/letsencrypt/pebble) is a small ACME server for tests. `make test-acme` starts Pebble in Docker, gets a certificate through TLS-ALPN-01 and another through HTTP-01, and removes the container:

```sh
make test-acme
```

The test needs a running Docker daemon in which `host-gateway` reaches the host's loopback address, as colima and Docker Desktop do. It uses host ports 14000, 14001, 14002 and 15000, and a container named `stagely-pebble`, so run one at a time. It is not part of `make check` for these reasons.

To run the gateway itself against Pebble, let Pebble accept every challenge without a check:

```sh
docker run -d --name pebble -p 127.0.0.1:14000:14000 -p 127.0.0.1:15000:15000 \
  -e PEBBLE_VA_ALWAYS_VALID=1 ghcr.io/letsencrypt/pebble:2.10.1
docker cp pebble:/test/certs/pebble.minica.pem /tmp/pebble.minica.pem
go run ./cmd/stagely-gateway run -enrollment-token <gateway token> -data-dir /tmp/stagely-gateway-pebble \
  -acme -acme-directory https://localhost:14000/dir -acme-directory-ca /tmp/pebble.minica.pem \
  -listen :8443 -http-listen :8081
```

Pebble signs certificates with a new root on each start. Get it from `https://localhost:15000/roots/0` to check a certificate, for example with `curl --cacert`.

## Route health

The gateway checks each route and reports the result to the control plane in `route_health` messages. A check finds two separate things: whether the gateway reaches the stagelet's public port, and whether the app answers its health path.

- It checks right after it applies a route table, and then every 20 seconds. At most 16 checks run at the same time.
- First it opens a TCP connection to the stagelet's upstream address, with a 3-second timeout. A connection means the route is reachable.
- Then it sends `GET <health path>` to the upstream, with the stagelet's host name in the `Host` header and a 5-second timeout. The control plane sends the health path from `.stagely.yml` with each route. The app is healthy when it answers with a 2xx status. The gateway does not follow redirects, so a 3xx answer counts as unhealthy, as it does in the agent's check during a deploy. The request carries no visitor cookies, and the gateway reads at most 64 KiB of response headers and none of the body.
- The app sees this request every 20 seconds in its logs, with the user agent `stagely-gateway`.
- Each state holds the stagelet ID, the upstream address, whether the gateway reached it and the error if it did not, whether the app is healthy and the reason if it is not, such as `GET /healthz: status 503`, and the check time.
- On each control plane session, the first report holds every route. After that, a report holds only new routes and routes whose reachability, health, error or reason changed, such as a 500 after a 503. A reason names no network address, because the local port changes with every request. One message holds at most 100 routes; a larger report uses more messages.

## When the gateway loses the control plane

The gateway asks the control plane every 20 seconds which visitor sessions are still valid. It refuses a session that it could not confirm in the last 50 seconds. Each question waits at most 10 seconds for an answer, so a lost or slow answer does not hold up the next question. When the control plane answers that it could not check the sessions, the gateway confirms none of them and revokes none, and asks again 20 seconds later. If the control plane is unreachable or keeps failing, the gateway refuses all sessions within about 50 seconds, and no new visitor can enter, because only the control plane can redeem a handoff. When the control plane is back, the gateway confirms the sessions again, and the sessions that are still valid work again.

## Open requests

Some requests stay open for a long time, such as WebSocket connections and streaming responses. The gateway ends all open requests of a session in these cases:

- The control plane answers that the session is no longer valid, for example because the visitor lost access. The requests end when the answer arrives, at most about 20 seconds after the control plane revoked the session.
- The gateway could not confirm the session in the last 50 seconds. The gateway checks this at each question, so the requests end at most 20 seconds after the 50 seconds pass.
- The session expired. The requests end at the next question.

The requests of other sessions continue.

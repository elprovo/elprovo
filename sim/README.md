# stagely-sim

Local simulations of the outside services Stagely depends on, behind their real API contracts. Stagely's own code talks to each service through its documented HTTP API with a configurable base URL; pointing it at `stagely-sim` instead of the real service is configuration only.

This is a separate Go module. No Stagely product binary imports it.

## Run

```sh
cd sim && go run ./cmd/stagely-sim -h
```

`-data-dir` keeps each simulation's state as JSON, so a restart keeps every object.

`sim/Dockerfile` builds a `stagely-sim` image with the Docker CLI and git with its HTTP backend, for running the simulations inside a Compose project. Its state lives in the `/data` volume. A simulated cloud creates its machines with the Docker CLI, so mount the Docker socket:

```sh
docker build -t stagely-sim sim
docker run --rm -v /var/run/docker.sock:/var/run/docker.sock -p 127.0.0.1:9300:9300 \
  stagely-sim -digitalocean -do-addr :9300
```

## Simulations

Each simulation lives in `internal/<service>/`, registers its flags in `cmd/stagely-sim/simulations.go`, and documents below which official sources it follows and what it covers.

| Service | Package | Follows |
| --- | --- | --- |
| Stripe | `internal/stripe` | Stripe API reference for API version `2026-09-30.endive`, the webhook signature docs, and stripe-go v87 and stripe-node for wire formats. See [Stripe](#stripe). |
| GitHub | `internal/github` | GitHub REST API reference for version `2022-11-28`, the GitHub App authentication docs, the webhook docs, google/go-github for JSON shapes, and `git http-backend`. See [GitHub](#github). |
| Machines | `internal/machines`, `vm/` | cloud-init 26.1, the version Ubuntu 24.04 ships, for NoCloud seed data, cloud-config modules, `cloud-init status` and `cloud-init clean`; systemd for the unit directives listed. Every simulated cloud runs its servers as machines. See [Machines](#machines). |
| DigitalOcean | `internal/digitalocean` | DigitalOcean API v2 reference and OpenAPI spec, godo v1.216.0 (the version the Terraform provider vendors) and terraform-provider-digitalocean v2.105.0 for wire formats. See [DigitalOcean](#digitalocean). |
| AWS | `internal/aws` | The EC2 API reference (Query API version `2016-11-15`), the Signature Version 4 docs and test suite, the STS and ECR API references, aws-sdk-go-v2 service/ec2 v1.332.0, service/ecr v1.65.0 and service/sts v1.51.0 for wire formats, terraform-provider-aws v6.66.0 and packer-plugin-amazon v1.8.2 for the calls they make. See [AWS](#aws). |
| Google Cloud | `internal/gcp` | Compute Engine API v1 reference and its discovery document in google-api-go-client v0.298.0, the OAuth 2.0 service account flow, the Artifact Registry API v1 and Docker authentication docs, terraform-provider-google v8.4.0 and packer-plugin-googlecompute v1.2.7 for wire formats. See [Google Cloud](#google-cloud). |
| Hetzner Cloud | `internal/hetzner` | Hetzner Cloud API reference and its OpenAPI spec, hcloud-go v2.48.0 (the version the Terraform provider and the Packer plugin vendor), terraform-provider-hcloud v1.69.0 and packer-plugin-hcloud v1.8.1 for wire formats, and cloud-init's Hetzner datasource for instance metadata. See [Hetzner Cloud](#hetzner-cloud). |
| Vultr | `internal/vultr` | Vultr API v2 reference and its OpenAPI document, govultr v3.31.2 (the version the Terraform provider vendors), terraform-provider-vultr v2.32.0 and packer-plugin-vultr v2.7.0 for wire formats, and the Vultr metadata API. See [Vultr](#vultr). |
| Azure | `internal/azure` | Azure Resource Manager REST reference and its OpenAPI specs (azure-rest-api-specs), the Microsoft identity platform client credentials docs, the Instance Metadata Service docs, hashicorp/go-azure-sdk v0.20260917.1142820 (the version terraform-provider-azurerm v5.7.0 vendors) and packer-plugin-azure v2.6.4 for wire formats. See [Azure](#azure). |

## Stripe

The Stripe simulation serves the calls Stagely makes to sell one subscription plan: a hosted Checkout page, subscriptions with invoices and payments, the billing portal, test clocks, and events delivered to signed webhooks. It runs two listeners: the API, like `api.stripe.com`, and the hosted pages, like `checkout.stripe.com` and `billing.stripe.com`. The hosted-pages listener also serves the `/_sim/` admin API.

```sh
go run ./cmd/stagely-sim -stripe \
  -stripe-webhook url=http://localhost:8080/webhooks/stripe,secret=whsec_local
```

Point a Stripe SDK at `http://localhost:9200` with the key `sk_test_stagely`. In stripe-go, set the API backend URL with `stripe.GetBackendWithConfig`. In stripe-node, set `host`, `port` and `protocol`. State is kept in `<data-dir>/stripe/state.json`.

### Flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `-stripe` | off | Run the Stripe simulation |
| `-stripe-api-addr` | `:9200` | Listen address of the API |
| `-stripe-web-addr` | `:9201` | Listen address of the hosted pages and the admin API |
| `-stripe-web-url` | `http://localhost:9201` | Public base URL of the hosted pages. Checkout Session, portal session and invoice URLs use it. |
| `-stripe-secret-key` | `sk_test_stagely` | Secret key that the API accepts. Repeat the flag to accept several keys. Each key must start with `sk_test_`. |
| `-stripe-api-version` | `2026-09-30.endive` | API version written into events. stripe-go's `webhook.ConstructEvent` rejects an event whose release train (`endive`) differs from the SDK's, so match it to the SDK. |
| `-stripe-plan-name` | `Team Stagelet` | Name of the seeded product `prod_team_stagelet` |
| `-stripe-plan-amount` | `2000` | Amount of the seeded price `price_team_stagelet`, in the smallest currency unit |
| `-stripe-plan-currency` | `usd` | Currency of the seeded price |
| `-stripe-plan-interval` | `month` | Interval of the seeded price: `day`, `week`, `month` or `year` |
| `-stripe-webhook` | none | Webhook endpoint that receives every event, as `url=...,secret=whsec_...`. Repeatable. |
| `-stripe-mark-unpaid` | off | When every payment retry fails, mark the subscription `unpaid` instead of canceling it |

The seeded price has the lookup key `team_stagelet`, so code can find it with `GET /v1/prices?lookup_keys[]=team_stagelet` in the simulation and in Stripe alike. Every start updates the seeded product and price from the flags.

### Coverage

| Endpoint | Notes |
| --- | --- |
| `POST /v1/customers`, `GET /v1/customers`, `GET`/`POST /v1/customers/{id}` | `email`, `name`, `description`, `phone`, `metadata`, `invoice_settings[default_payment_method]`, `test_clock`. List filters: `email`, `test_clock`. |
| `POST`/`GET /v1/products`, `GET /v1/products/{id}` | `name`, `description`, `active`, `metadata` |
| `POST`/`GET /v1/prices`, `GET /v1/prices/{id}` | `product` or `product_data[name]`, `unit_amount`, `currency`, `recurring[interval]`, `recurring[interval_count]`, `lookup_key`, `nickname`. List filters: `active`, `product`, `type`, `lookup_keys[]`. |
| `POST`/`GET /v1/checkout/sessions`, `GET /v1/checkout/sessions/{id}`, `POST .../{id}/expire` | `mode=subscription` only. `customer` or `customer_email`, `line_items`, `success_url` with `{CHECKOUT_SESSION_ID}`, `cancel_url`, `client_reference_id`, `metadata`, `subscription_data[metadata]`, `expires_at` (30 minutes to 24 hours). Open sessions expire on their own within 5 seconds of `expires_at`. |
| `GET /v1/subscriptions`, `GET`/`POST`/`DELETE /v1/subscriptions/{id}` | Update `cancel_at_period_end` and `metadata`; `DELETE` cancels now. List filters: `customer`, `price`, and `status`, where an empty status excludes canceled subscriptions and `all` includes them. |
| `GET /v1/invoices`, `GET /v1/invoices/{id}` | List filters: `customer`, `subscription`, `status` |
| `GET /v1/payment_intents/{id}`, `GET /v1/payment_methods/{id}` | Read only |
| `POST /v1/billing_portal/sessions` | `customer`, `return_url` |
| `POST`/`GET /v1/test_helpers/test_clocks`, `GET .../{id}`, `POST .../{id}/advance` | `frozen_time`, `name` |
| `GET /v1/events`, `GET /v1/events/{id}` | List filters: `type` (a trailing `*` matches a prefix) and `types[]`. Events older than 30 days are dropped. |
| `POST`/`GET /v1/webhook_endpoints`, `GET`/`POST`/`DELETE /v1/webhook_endpoints/{id}` | `url`, `enabled_events[]`, `api_version`, `description`, `metadata`, `disabled`. Only create returns `secret`. |

Every API request behaves as follows:

- **Authentication.** `Authorization: Bearer sk_test_...`, or Basic auth with the key as the user name. A missing or unknown key gets a 401 with Stripe's error body.
- **Form encoding.** Bracketed keys decode into nested values: `metadata[k]=v`, `line_items[0][price]=...`, and both `expand[]=x` (curl) and `expand[0]=x` (SDKs). `metadata[k]=` removes a key and `metadata=` removes them all. Parameters the simulation does not model are ignored.
- **Errors.** `{"error": {"type", "code", "message", "param", "doc_url"}}` with Stripe's status codes: 400, 401, 402 for card errors, 404, 409.
- **Pagination.** Lists return `object`, `data`, `has_more` and `url`, newest first, with `limit` (1 to 100, default 10), `starting_after` and `ending_before`.
- **Expansion.** `expand[]` replaces an ID with its object at any depth up to 4, including lists (`data.subscription`), nested hashes (`invoice_settings.default_payment_method`) and chains (`latest_invoice.payment_intent`). A path that names no ID is left as it is, where Stripe returns an error.
- **Idempotency.** On `POST`, an `Idempotency-Key` replays the first response for 24 hours with `Idempotent-Replayed: true`. A reused key with other parameters or another endpoint gets a 400 `idempotency_error`. A key that is in use by a concurrent request gets a 409. Responses with status 2xx, 402 and 5xx are kept; other 4xx responses are validation errors and are not kept, as in Stripe.
- **Response shapes.** Objects have the fields of API version `2026-09-30.endive`. They also carry the older fields that this version moved, so SDKs pinned to either side decode them: subscriptions have `current_period_start`/`current_period_end` at the top level and on each item, and invoices have `subscription`, `payment_intent` and `paid` as well as `parent.subscription_details` and `payments`.

### Checkout, billing and test cards

The hosted Checkout page shows the plan, the price, the email (read-only when the session already knows it), card fields and a **Subscribe** button, as Stripe's page does in subscription mode. A successful payment redirects to `success_url`. A decline keeps the session open and shows Stripe's message, for example "Your card was declined."

| Card | Result |
| --- | --- |
| `4242 4242 4242 4242` | Succeeds |
| `4000 0000 0000 0002` | Declined at Checkout and in the portal (`card_declined`, `generic_decline`) |
| `4000 0000 0000 0341` | Attaches and pays the first invoice; every renewal or retry is declined |

Use any future expiration date and any three- or four-digit security code. Stripe also declines the first payment for `4000 0000 0000 0341` in Checkout. The simulation lets that payment succeed, so renewal failures can be tested without a trial.

Completing Checkout creates the customer if the session has none, a card payment method, an `incomplete` subscription, an invoice and its payment intent. It pays the invoice, makes the subscription `active` and completes the session. The events, in this order: `customer.created`, `payment_method.attached`, `customer.subscription.created`, `invoice.created`, `invoice.finalized`, `payment_intent.created`, `payment_intent.succeeded`, `invoice.paid`, `invoice.payment_succeeded`, `customer.subscription.updated` (status `incomplete` to `active`) and `checkout.session.completed`. Each event's `data.object` is the object at that moment, and `*.updated` events carry `data.previous_attributes`.

The billing portal page shows each active plan with its price, status and renewal date, the payment method and the invoice history. Its actions:

- **Cancel plan** sets `cancel_at_period_end` and `cancel_at` and emits `customer.subscription.updated`.
- **Renew plan** clears them and emits `customer.subscription.updated`.
- **Update payment method** attaches a new card, makes it the customer's and the subscriptions' default, and emits `payment_method.attached`, `customer.updated` and `customer.subscription.updated`.
- **Return to ...** links to `return_url`.

Each finalized invoice has a hosted page at `hosted_invoice_url`.

### Test clocks and dunning

A customer created with `test_clock` follows that clock. Its subscriptions and invoices use the clock's frozen time, and renewals happen only when the clock advances. The subscriptions of customers without a clock never renew. `POST .../advance` processes every due period end and payment retry in time order. It answers with `status: advancing` and leaves the clock `ready`. Stripe's limit applies: at most two intervals of the shortest subscription ahead, or two years when the clock has none.

At a period end, the simulation does one of the following:

- A subscription with `cancel_at_period_end` becomes `canceled` and emits `customer.subscription.deleted`.
- Otherwise it starts a new period and creates, finalizes and charges a `subscription_cycle` invoice.
- If the charge fails, the invoice stays `open`, `payment_intent.payment_failed` and `invoice.payment_failed` are emitted, and an `active` subscription becomes `past_due`.

The dunning rule: the simulation retries a failed invoice 3, 5 and 7 days after its first attempt (`next_payment_attempt` shows the next retry). A successful retry pays the invoice and makes the subscription `active` again. When the fourth attempt fails, the subscription is canceled with `cancellation_details.reason: payment_failed`, or marked `unpaid` with `-stripe-mark-unpaid`. An `unpaid` subscription creates no further invoices.

### Webhooks

Every state change records an event and queues it for each enabled endpoint whose `enabled_events` holds the event type or `*`. Endpoints come from `-stripe-webhook` or `POST /v1/webhook_endpoints`. A flag endpoint receives every event and carries the metadata `stagely_sim_flag: true`. At each start, the simulation deletes the flag endpoints that the flags no longer name. Endpoints created through the API stay until they are deleted. A delivery is a `POST` with the event JSON and the header `Stripe-Signature: t=<unix time>,v1=<hex HMAC-SHA256 of "<t>.<body>" keyed with the whole whsec_ secret>`. Each attempt is signed again with a new timestamp.

Deliveries run in the background, four at a time, so their order is not guaranteed, as in Stripe. Each attempt has a 10-second timeout. Any 2xx status is a success; redirects and other statuses are failures. A failed delivery is retried 1 second, 10 seconds, 1 minute and 10 minutes later. After five attempts it gives up. A delivery to an endpoint that was disabled or deleted stops. The queue is in memory: deliveries that are still pending when the simulation stops are lost. To send them again, use the redelivery admin call.

### Admin API

These calls are on the hosted-pages listener and need no key. Errors use Stripe's error body.

List every object, grouped by type:

```sh
curl http://localhost:9201/_sim/objects
```

Make the next automatic charge for a customer fail, once. This affects a renewal or a retry, not Checkout:

```sh
curl -X POST http://localhost:9201/_sim/customers/cus_123/fail_next_payment
```

Deliver an event again to every endpoint that receives it, or to one endpoint:

```sh
curl -X POST http://localhost:9201/_sim/events/evt_123/redeliver -d endpoint=we_123
```

Complete a Checkout Session without the browser. `card` defaults to `4242424242424242`, `expiry` to `12/34` and `cvc` to `123`. `email` is needed when the session has no customer email. A decline answers 402 with a `card_error`:

```sh
curl -X POST http://localhost:9201/_sim/checkout/sessions/cs_test_123/complete \
  -d card=4000000000000341 -d email=ada@example.com -d name="Ada Lovelace"
```

### Differences from Stripe

- Only `mode=subscription` Checkout with recurring prices. No trials, coupons, taxes, proration, plan changes, one-time payments or charges (`latest_charge` is null).
- A declined Checkout attempt creates no objects and no events.
- A renewal invoice is finalized and charged at the period end, not about an hour later.
- Portal sessions do not expire, the portal has no cancellation confirmation step, and test clocks are never deleted after `deletes_after`.
- Events are rendered once, in `-stripe-api-version`. An endpoint's `api_version` changes only the `api_version` field of the payload it receives.

### Sources

- API reference, version `2026-09-30.endive`: [errors](https://docs.stripe.com/api/errors), [pagination](https://docs.stripe.com/api/pagination), [expanding responses](https://docs.stripe.com/api/expanding_objects), [idempotent requests](https://docs.stripe.com/api/idempotent_requests), [customers](https://docs.stripe.com/api/customers/object), [Checkout Sessions](https://docs.stripe.com/api/checkout/sessions/object), [creating a Checkout Session](https://docs.stripe.com/api/checkout/sessions/create), [subscriptions](https://docs.stripe.com/api/subscriptions/object), [invoices](https://docs.stripe.com/api/invoices/object), [invoice line items](https://docs.stripe.com/api/invoices/line_item), [PaymentIntents](https://docs.stripe.com/api/payment_intents/object), [events](https://docs.stripe.com/api/events/object), [webhook endpoints](https://docs.stripe.com/api/webhook_endpoints/create), [billing portal sessions](https://docs.stripe.com/api/customer_portal/sessions/object), [test clocks](https://docs.stripe.com/api/test_clocks/object) and [advancing them](https://docs.stripe.com/api/test_clocks/advance)
- Guides: [webhooks and signature verification](https://docs.stripe.com/webhooks), [subscription webhooks and statuses](https://docs.stripe.com/billing/subscriptions/webhooks), [simulating subscriptions with test clocks](https://docs.stripe.com/billing/testing/test-clocks/simulate-subscriptions), [test cards](https://docs.stripe.com/testing), [error codes](https://docs.stripe.com/error-codes), [low-level error handling and idempotency](https://docs.stripe.com/error-low-level), [API versions](https://docs.stripe.com/upgrades)
- SDK source: [stripe-go `api_version.go`](https://github.com/stripe/stripe-go/blob/master/api_version.go) (pins `2026-09-30.endive`), [stripe-go `form`](https://github.com/stripe/stripe-go/blob/master/form/form.go) (indexed array keys), [stripe-go `webhook`](https://github.com/stripe/stripe-go/blob/master/webhook/client.go) (signature and API version check), [stripe-node `utils.ts`](https://github.com/stripe/stripe-node/blob/master/src/utils.ts) (form encoding)

## GitHub

The GitHub simulation serves the calls Stagely makes to GitHub: GitHub App authentication and installation tokens, sign-in through the GitHub App web application flow, the app installation flow, the REST endpoints that read plan inputs and report results on pull requests, and signed webhooks. Repositories are real bare git repositories, so `git clone` and `git push` work. It runs two listeners: the web host, like `github.com`, and the API host, like `api.github.com`. The web host also serves the `/_sim/` admin API.

```sh
go run ./cmd/stagely-sim -github \
  -github-default-user octo \
  -github-default-app-name "Stagely Local" \
  -github-default-app-webhook-url http://localhost:8080/webhooks/github \
  -github-default-app-callback-urls http://localhost:8080/auth/github/callback \
  -github-default-app-setup-url http://localhost:8080/github/setup
```

Point Stagely's GitHub web base URL at `http://localhost:9100` and its API base URL at `http://localhost:9101`. State is kept in `<data-dir>/github/state.json`, bare repositories in `<data-dir>/github/repos/<owner>/<repo>.git`, and each app's private key in `<data-dir>/github/apps/<slug>.private-key.pem`.

### Flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `-github` | off | Run the GitHub simulation |
| `-github-web-addr` | `:9100` | Listen address of the web host |
| `-github-api-addr` | `:9101` | Listen address of the API host |
| `-github-web-url` | `http://localhost:9100` | Public base URL of the web host. Links, redirects, clone URLs and archive URLs use it. Change it when the simulation runs in a container. |
| `-github-api-url` | `http://localhost:9101` | Public base URL of the API host. `url` fields and `Link` headers use it. |
| `-github-default-user` | none | Login of a user to create at startup |
| `-github-default-app-name` | none | Name of a GitHub App to create at startup, owned by the default user. Its slug is the name in lowercase with dashes. |
| `-github-default-app-webhook-url` | none | Webhook URL of the default app. Without it, the app gets no deliveries. |
| `-github-default-app-webhook-secret` | generated | Webhook secret of the default app |
| `-github-default-app-callback-urls` | none | Comma-separated user authorization callback URLs of the default app |
| `-github-default-app-setup-url` | none | Setup URL of the default app |

Every start updates the default app's webhook URL, callback URLs and setup URL from the flags, and the webhook secret when the flag is set. The app keeps its id, client ID, client secret and key. Read them with `curl http://localhost:9100/_sim/apps/<slug>`.

### API host

| Endpoint | Credentials | Notes |
| --- | --- | --- |
| `GET /app` | App JWT | Includes `installations_count` |
| `GET /app/installations`, `GET`/`DELETE /app/installations/{id}` | App JWT | `DELETE` answers 202, revokes the installation's tokens and sends `installation` `deleted` |
| `PUT`/`DELETE /app/installations/{id}/suspended` | App JWT | Sends `installation` `suspend` or `unsuspend`. A suspended installation cannot get tokens; suspending revokes its tokens. |
| `POST /app/installations/{id}/access_tokens` | App JWT | Answers 201 with a `ghs_` token that expires in one hour, `permissions` and `repository_selection`. `repositories` (names) and `repository_ids` narrow the token and add `repositories` to the answer; `permissions` can only lower the installation's. |
| `GET /installation/repositories` | Installation token | `total_count`, `repository_selection`, `repositories` |
| `GET /user/installations`, `GET /user/installations/{id}/repositories` | User token | Installations of the token's app on the user's account or on an organization the user belongs to, and their repositories the user can access, with `permissions` |
| `GET /user` | User or personal token | Includes `email` |
| `GET /users/{login}` | None | Users and organizations |
| `GET /repos/{owner}/{repo}` | Any | Read access. User and personal tokens get `permissions`. |
| `GET /repos/{owner}/{repo}/pulls`, `GET .../pulls/{number}` | `pull_requests: read` | `state` is `open` (default), `closed` or `all`; newest first |
| `GET /repos/{owner}/{repo}/contents/{path}` | `contents: read` | `ref` defaults to the default branch. JSON with base64 `content` for files, an array for directories, raw bytes for `Accept: application/vnd.github.raw+json` or `application/vnd.github.raw`. Files larger than 1 MB need the raw media type, as on GitHub. |
| `GET /repos/{owner}/{repo}/tarball/{ref}` | `contents: read` | 302 to `<web-url>/codeload/...`, a link that works without credentials for 5 minutes. The archive's top folder is `<owner>-<repo>-<7-character sha>/`. |
| `GET /repos/{owner}/{repo}/commits/{ref}` | `contents: read` | `author` and `committer` are sim users matched by email. No `files` or `stats`. |
| `GET /repos/{owner}/{repo}/collaborators/{username}/permission` | Any with access | `permission` is `admin`, `write`, `read` or `none` |
| `GET`/`POST /repos/{owner}/{repo}/issues/{number}/comments` | `issues` or `pull_requests` | `POST` needs `write` and sends `issue_comment` `created` |
| `PATCH`/`DELETE /repos/{owner}/{repo}/issues/comments/{id}` | `write`, or the comment's author | Send `issue_comment` `edited` (with `changes.body.from`) or `deleted` |
| `GET`/`POST`/`DELETE /repos/{owner}/{repo}/issues/{number}/labels`, `DELETE .../labels/{name}` | `issues` or `pull_requests` | `POST` takes `{"labels": [...]}` or a bare array and creates unknown labels. Each change sends `pull_request` `labeled` or `unlabeled`. |
| `POST /repos/{owner}/{repo}/statuses/{sha}` | `statuses: write` | `state` is `error`, `failure`, `pending` or `success`; `context` defaults to `default` |
| `POST /repos/{owner}/{repo}/deployments` | `deployments: write` | Without `required_contexts`, every status context of the commit must be `success`, otherwise 409. `environment` defaults to `production`. |
| `POST /repos/{owner}/{repo}/deployments/{id}/statuses` | `deployments: write` | `state` is one of GitHub's seven deployment states |

Every API request behaves as follows:

- **Credentials.** `Authorization: Bearer <token>` or `token <token>`. A token with three dot-separated parts is an app JWT: it must be signed with RS256 by the app's key, `iss` must be the app's client ID or numeric id, `iat` must not be in the future, and `exp` must be in the future but at most 10 minutes ahead. GitHub's messages explain each failure, with status 401. Other tokens are `ghs_` installation tokens, `ghu_` user tokens and `ghp_` personal tokens. An unknown or expired token gets 401 `Bad credentials`.
- **Access.** An installation token reaches only the repositories of its installation, minus any narrowing, with the installation's permissions. A user token reaches repositories that both the user and an installation of its app can reach, with the installation's permissions limited by the user's role. A personal token has the user's access. Public repositories are readable by everyone. A private repository the caller cannot reach answers 404; a missing permission answers 403 `Resource not accessible by integration` for app tokens.
- **Roles.** The owner of a user repository and the admins of an organization have `admin`. Organization members have `read`, GitHub's default base permission. Collaborators have the permission set through the admin API.
- **Errors.** `{"message", "documentation_url", "status"}`, plus `errors` for 422 `Validation Failed`.
- **Rate limits.** Every answer carries `X-RateLimit-Limit`, `-Remaining`, `-Used`, `-Reset` and `-Resource: core`. Each token, app and anonymous client address has its own hourly budget: 5000 requests when authenticated, 60 without credentials. An exhausted budget answers 403 `API rate limit exceeded for ...`.
- **Pagination.** List endpoints take `per_page` (default 30, at most 100) and `page`, and send a `Link` header with `prev`, `next`, `last` and `first` as GitHub does.

### Web host

- **Sign-in.** `/login` lists the sim users with a **Sign in as** button each, and a form that creates a user. There are no passwords. The session cookie is `user_session`.
- **User authorization.** `GET /login/oauth/authorize?client_id&redirect_uri&state` signs the user in if needed, shows **Authorize \<app name\>** the first time, then redirects to `redirect_uri` with `code` and `state`. `redirect_uri` must equal one of the app's callback URLs; without it, the first one is used. A different `redirect_uri` redirects to the first callback URL with `error=redirect_uri_mismatch`. **Cancel** redirects with `error=access_denied`. A code works once, for 10 minutes.
- **Token exchange.** `POST /login/oauth/access_token` takes form or JSON parameters, and the client credentials in the body or a Basic auth header. With `Accept: application/json` it answers JSON, otherwise form encoding. It returns `access_token` (`ghu_`), `expires_in` 28800, `refresh_token` (`ghr_`), `refresh_token_expires_in` 15897600, `scope` "" and `token_type` `bearer`. `grant_type=refresh_token` returns a new pair and revokes the old pair. Errors answer 200 with `error`, `error_description` and `error_uri`: `incorrect_client_credentials`, `bad_verification_code`, `redirect_uri_mismatch` and `bad_refresh_token`.
- **Repositories.** The home page lists the repositories the signed-in user can see and their organizations. `/new` creates a repository under the user's own account or under an organization the user administers, private by default. With **Add a README file** it gets a `main` branch with a README; without it, it starts empty, and its page shows how to push to it. An installation on all repositories covers new repositories at once.
- **People.** `/orgs/<org>/people` lists an organization's members to its members. Admins add a user as a member or an admin, change a role, or remove a member. The last admin can't be removed or made a member.
- **Installation.** `GET /apps/<slug>/installations/new?state=...` lets the signed-in user pick their own account or an organization they administer, and all or selected repositories. Submitting creates the installation and sends `installation` `created`, or updates it and sends `installation_repositories` `added` and `removed`. It then redirects to the setup URL with `installation_id`, `setup_action=install` (or `update`) and `state`.
- **Repositories.** `/` lists the repositories you can see. `/<owner>/<repo>` lists pull requests and has a form to open one from a branch. `/<owner>/<repo>/pull/<n>` shows comments and labels, and lets the signed-in user comment, add and remove labels, close, reopen and merge. Merging writes a real merge commit to the base branch, sends `pull_request` `closed` with `merged: true`, and sends `push` for the base branch. Actions other than commenting need write access.
- **Git.** `/<owner>/<repo>.git` serves clone, fetch and push through `git http-backend`. Use any password with a sim user's login, or a `ghp_`, `ghu_` or `ghs_` token as the password. Push needs write access. A push by a user or with a personal token to a repository that does not exist creates it, private, when the pusher owns the account or administers the organization. Each changed branch or tag sends `push`, and a pushed branch that is the head of open pull requests sends `pull_request` `synchronize` with `before` and `after`.

### Webhooks

Each event goes to every installation that covers the repository and whose app subscribes to the event; `installation` and `installation_repositories` always go to their app, and `ping` goes to a new app with a webhook URL. Apps subscribe to `pull_request`, `issue_comment` and `push` unless created with other `events`. Payloads follow GitHub's shapes, with `action`, `repository`, `organization` for organization repositories, `installation` (`{"id"}`, or the full object for installation events) and `sender`:

| Event | Actions |
| --- | --- |
| `ping` | none |
| `installation` | `created`, `deleted`, `suspend`, `unsuspend` |
| `installation_repositories` | `added`, `removed` |
| `pull_request` | `opened`, `reopened`, `synchronize`, `closed` (with `merged`), `labeled`, `unlabeled`, `edited` (with `changes`) |
| `issue_comment` | `created`, `edited`, `deleted`. `issue.pull_request` is always set, because every issue is a pull request. |
| `push` | none. `created`, `deleted`, `forced`, `commits` with `added`, `removed` and `modified`, `head_commit`, `compare` and `pusher`. |

A delivery is a `POST` with `Content-Type: application/json`, `User-Agent: GitHub-Hookshot/stagely-sim`, `X-GitHub-Event`, `X-GitHub-Delivery` (a GUID), `X-GitHub-Hook-ID`, `X-GitHub-Hook-Installation-Target-Type: integration`, `X-GitHub-Hook-Installation-Target-ID` (the app id), `X-Hub-Signature-256: sha256=<hex HMAC-SHA256 of the body keyed with the webhook secret>` and the older `X-Hub-Signature: sha1=...`. Deliveries run in the background, four at a time, with a 10-second timeout, so their order is not guaranteed. Like GitHub, the simulation does not retry a failed delivery. The last 1000 deliveries are logged with their result; a redelivery keeps the original `X-GitHub-Delivery` GUID.

### Admin API

These calls are on the web host, take and return JSON, and need no credentials. Do not expose the web host outside your machine: the admin API can read any local directory into a repository. Errors use GitHub's error body.

Create a user, an organization, and make the user an admin (`role` is `admin` or `member`):

```sh
curl -X POST localhost:9100/_sim/users -d '{"login":"octo","name":"Octo Cat","email":"octo@example.com"}'
curl -X POST localhost:9100/_sim/orgs -d '{"login":"acme"}'
curl -X PUT localhost:9100/_sim/orgs/acme/members/octo -d '{"role":"admin"}'
```

Create a repository: empty, with a first commit of a local directory's files (`.gitignore` applies), or with only a README when `auto_init` is true and there is no `source_dir`. `private` defaults to false and `default_branch` to `main`. Give another user access (`permission` is `read`, `write` or `admin`):

```sh
curl -X POST localhost:9100/_sim/repos -d '{"owner":"acme","name":"web","private":true,"source_dir":"/path/to/project"}'
curl -X PUT localhost:9100/_sim/repos/acme/web/collaborators/mona -d '{"permission":"write"}'
```

Create a GitHub App. The answer has the app id, slug, client ID and secret, webhook secret, the private key PEM and the path of the key file. `permissions` defaults to `metadata` and `contents` read, and `pull_requests`, `issues`, `statuses` and `deployments` write. Read an app's credentials again by slug:

```sh
curl -X POST localhost:9100/_sim/apps -d '{"name":"Stagely Dev","owner":"octo","webhook_url":"http://localhost:8080/webhooks/github","callback_urls":["http://localhost:8080/auth/github/callback"],"setup_url":"http://localhost:8080/github/setup"}'
curl localhost:9100/_sim/apps/stagely-dev
```

Install an app on an account without the browser. `repository_selection` is `all` or `selected`, with repository names. It answers 201 for a new installation and 200 for an update, and sends the same webhooks as the installation page:

```sh
curl -X POST localhost:9100/_sim/apps/stagely-dev/installations -d '{"account":"acme","repository_selection":"selected","repositories":["web"]}'
```

Create a personal token for a user, for git and API calls with the user's access:

```sh
curl -X POST localhost:9100/_sim/users/octo/tokens
```

Act on pull requests as a user. Opening needs a pushed head branch with commits that the base branch lacks; `base` defaults to the default branch. `PATCH` changes `title`, `body` and `state` (`open` or `closed`):

```sh
curl -X POST localhost:9100/_sim/repos/acme/web/pulls -d '{"user":"octo","head":"feature","title":"Add feature","body":"Details"}'
curl -X PATCH localhost:9100/_sim/repos/acme/web/pulls/1 -d '{"user":"octo","state":"closed"}'
curl -X PUT localhost:9100/_sim/repos/acme/web/pulls/1/merge -d '{"user":"octo"}'
curl -X POST localhost:9100/_sim/repos/acme/web/issues/1/comments -d '{"user":"mona","body":"/stagely deploy"}'
curl -X POST localhost:9100/_sim/repos/acme/web/issues/1/labels -d '{"user":"octo","labels":["stagelet"]}'
curl -X DELETE 'localhost:9100/_sim/repos/acme/web/issues/1/labels/stagelet?user=octo'
```

List deliveries, newest first, optionally for one app, and deliver one again:

```sh
curl 'localhost:9100/_sim/deliveries?app=stagely-dev'
curl -X POST localhost:9100/_sim/deliveries/42/redeliver
```

### Differences from GitHub

- Issues exist only as pull requests. There are no forks, reviews, review comments, check runs, draft pull requests, teams, `maintain` or `triage` roles, or branch protection.
- Callback URLs match exactly; GitHub's optional wildcard matching is not simulated. `code_challenge` and `code_verifier` (PKCE) are accepted and not checked. The installation flow never starts the OAuth flow ("Request user authorization during installation").
- `setup_action` is not in GitHub's docs, but GitHub sends it, so the simulation does too.
- Contents: no `download_url` (always null), no `application/vnd.github.object` or `html` media types, symlinks are returned as `type: symlink` with `target` instead of the target file, and no 100 MB limit.
- Deployments: `auto_merge` and `auto_inactive` have no effect, and no `deployment` or `deployment_status` webhooks are sent.
- `avatar_url` is an inline `data:image/svg+xml` URL with the login's initial, not an HTTPS URL, so pages with a strict Content-Security-Policy can show it. `GET /avatars/u/{id}` on the web host serves the same image.
- Objects have no `node_id`. `GET /app/hook/deliveries` and the redelivery endpoint for apps are not served; use the admin API.
- A redelivery is signed with the app's current webhook secret and goes to its current webhook URL.
- A merge is always a merge commit. Commits pushed to a base branch do not mark pull requests as merged.

### Sources

- GitHub Apps authentication: [generating a JWT](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-a-json-web-token-jwt-for-a-github-app), [generating a user access token](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-a-user-access-token-for-a-github-app), [refreshing user access tokens](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/refreshing-user-access-tokens), [the user authorization callback URL](https://docs.github.com/en/apps/creating-github-apps/registering-a-github-app/about-the-user-authorization-callback-url), [token request errors](https://docs.github.com/en/apps/oauth-apps/maintaining-oauth-apps/troubleshooting-oauth-app-access-token-request-errors), [the setup URL](https://docs.github.com/en/apps/creating-github-apps/registering-a-github-app/about-the-setup-url), [sharing an app with `state`](https://docs.github.com/en/apps/sharing-github-apps/sharing-your-github-app)
- REST API, version `2022-11-28`: [GitHub Apps](https://docs.github.com/en/rest/apps/apps), [installations](https://docs.github.com/en/rest/apps/installations), [users](https://docs.github.com/en/rest/users/users), [repositories](https://docs.github.com/en/rest/repos/repos), [pull requests](https://docs.github.com/en/rest/pulls/pulls), [contents and archives](https://docs.github.com/en/rest/repos/contents), [commits](https://docs.github.com/en/rest/commits/commits), [collaborators](https://docs.github.com/en/rest/collaborators/collaborators), [issue comments](https://docs.github.com/en/rest/issues/comments), [labels](https://docs.github.com/en/rest/issues/labels), [commit statuses](https://docs.github.com/en/rest/commits/statuses), [deployments](https://docs.github.com/en/rest/deployments/deployments), [deployment statuses](https://docs.github.com/en/rest/deployments/statuses), [rate limits](https://docs.github.com/en/rest/using-the-rest-api/rate-limits-for-the-rest-api), [pagination](https://docs.github.com/en/rest/using-the-rest-api/using-pagination-in-the-rest-api), [troubleshooting](https://docs.github.com/en/rest/using-the-rest-api/troubleshooting-the-rest-api)
- Webhooks: [events and payloads](https://docs.github.com/en/webhooks/webhook-events-and-payloads), [validating deliveries](https://docs.github.com/en/webhooks/using-webhooks/validating-webhook-deliveries), [handling failed deliveries](https://docs.github.com/en/webhooks/using-webhooks/handling-failed-webhook-deliveries), [best practices](https://docs.github.com/en/webhooks/using-webhooks/best-practices-for-using-webhooks) (a redelivery keeps `X-GitHub-Delivery`)
- JSON shapes: google/go-github [`apps.go`](https://github.com/google/go-github/blob/master/github/apps.go), [`event_types.go`](https://github.com/google/go-github/blob/master/github/event_types.go), [`pulls.go`](https://github.com/google/go-github/blob/master/github/pulls.go), [`issues_comments.go`](https://github.com/google/go-github/blob/master/github/issues_comments.go), [`repos_contents.go`](https://github.com/google/go-github/blob/master/github/repos_contents.go), [`repos_deployments.go`](https://github.com/google/go-github/blob/master/github/repos_deployments.go)
- Git: [`git http-backend`](https://git-scm.com/docs/git-http-backend), [`git merge-tree --write-tree`](https://git-scm.com/docs/git-merge-tree)

## Machines

A machine is a Docker container that behaves like a fresh Ubuntu 24.04 cloud VM. The simulated clouds run their servers as machines: a DigitalOcean droplet or an EC2 instance is one machine. The `docker` CLI must work where `stagely-sim` runs.

The image is built from `vm/` the first time a machine is needed, and tagged `stagely-sim-vm:<hash of vm/>`. The first build downloads Ubuntu packages and takes a few minutes; later builds reuse Docker's cache. A change under `vm/` gives a new tag. The image has `openssh-server`, `docker.io`, `docker-compose-v2`, `docker-buildx`, `ca-certificates`, `curl`, `iproute2`, `iptables`, `iputils-arping`, `jq`, `python3-yaml` and `sudo`, which Ubuntu's cloud images also ship.

Each machine:

- runs privileged, so Docker works inside it, with Docker's init (tini) as PID 1 and `stagely-vm-init` under it;
- keeps `/var/lib/docker` on an anonymous volume, which is deleted with the machine;
- has `/run` on tmpfs, as on a VM;
- resolves the host names its cloud names, such as Google Cloud's `metadata.google.internal`, through `/etc/hosts`;
- has a private address on the Docker network the cloud names (DigitalOcean: `-do-network`, AWS: `-aws-network`, Google Cloud: `-gcp-network`, Hetzner: `-hcloud-network`, Vultr: `-vultr-network`) and a public address on the network with the same name and a `-public` suffix. The simulation creates both networks when they are missing. Addresses are static: they come from the top of each subnet, unless the cloud names the public address (Vultr: an instance's main reserved IP), and stay the same when the machine stops and starts.

`-machine-ca <file>` names a PEM file of CA certificates that every machine trusts, as a cloud image trusts the public CAs. The simulation copies the file into each machine as `/usr/local/share/ca-certificates/stagely-sim.crt`, and the machine adds it to its trust store before any service starts. Use it to give a simulated service, such as a container registry, a TLS certificate that machines accept.

To reach a machine, a container joins the private network for private addresses and the `-public` network for public addresses; the `-public` network plays the internet. On macOS (colima, Docker Desktop), container addresses are not reachable from the host, so publish ports instead (DigitalOcean: `-do-publish`), or run the client in a container on the networks.

### Boot

The simulation copies the NoCloud seed files into the container before it starts (with `docker cp`, so the data directory needs no file sharing with the Docker VM): `/var/lib/cloud/seed/nocloud/meta-data`, written as JSON, which is valid YAML, with `instance-id`, `local-hostname`, `local-ipv4`, `public-ipv4` and `public-keys`; and `/var/lib/cloud/seed/nocloud/user-data`. Then `stagely-vm-init`:

1. Applies the firewall the machine last received, before any service listens.
2. Runs the `init` stage: `bootcmd` on every boot; `write_files` and `ssh` once per instance.
3. Starts the units that `multi-user.target` and `default.target` want. The image enables `docker.service` (dockerd, ready when it reports `READY=1`) and `ssh.service`.
4. Runs the `config` stage, which saves `runcmd` as a script, and the `final` stage: packages, deferred `write_files`, the scripts (a `#!` user data script, then `runcmd`) and the final message.
5. On `docker stop`, stops every unit: `SIGTERM`, then `SIGKILL` after 10 seconds.

Per-instance modules record a semaphore in `/var/lib/cloud/instances/<instance-id>/sem/` before they run, as cloud-init does, so a reboot runs only `bootcmd`. A machine booted from a snapshot has a new instance id, so its per-instance modules run again.

### Cloud-config

| Key | What the machine does |
| --- | --- |
| `ssh_deletekeys` | Default `true`: deletes `/etc/ssh/ssh_host_*key*`. |
| `ssh_genkeytypes` | Key types to generate when `ssh_keys` is absent. Default `rsa`, `ecdsa`, `ed25519`; `[]` generates none. |
| `ssh_keys` | Writes `{rsa,ecdsa,ed25519}_private` to `/etc/ssh/ssh_host_<type>_key` (0600) and `_public` to `.pub` (0644), and derives a missing public key. With `ssh_keys`, no keys are generated. Certificates and other names are warnings. |
| `ssh_authorized_keys`, meta-data `public-keys` | Appended to root's `authorized_keys`. Root is the login user, as on DigitalOcean's images. |
| `disable_root` | `false` is accepted: root always signs in with these keys. `true` is a warning, because the machine has no default user to send root to. |
| `write_files` | `path`, `content`, `encoding` (`b64`, `base64`, `gz`, `gzip`, `gz+b64`, `gzip+base64` and the other cloud-init spellings), `permissions` (octal string or number), `owner` (default `root:root`, also given to new parent directories), `append`, `defer` (written in the final stage). An unknown encoding or invalid permissions is a warning and falls back to plain text and 0644. `source` is an error. |
| `packages` | Names or `[name, version]`. Packages that are installed already are skipped; the rest are installed with `apt-get` after a package index refresh. `cloud-init.log` lists what was skipped and installed. A failed install fails the module. |
| `package_update` | Runs `apt-get update`. A failure is logged at `ERROR`, as in cloud-init. `apt_update` does the same and is logged as `DEPRECATED`. |
| `bootcmd` | Runs on every boot as a `/bin/sh` script with `INSTANCE_ID` set. |
| `runcmd` | Saved as `/var/lib/cloud/instances/<id>/scripts/runcmd` in the config stage and run in the final stage. String items run as written; list items are quoted for the shell. |

User data that starts with `#!` is saved as `scripts/part-001` and runs before `runcmd`. Any other cloud-config key is logged as a warning (`cloud-config key "users" is not supported by the simulated VM; it was ignored`), and user data in another format (MIME multipart, gzip, `#include`, jinja templates) is a warning with nothing applied. A warning makes `cloud-init status` report a degraded result, so an unsupported setting never passes silently.

`cloud-init status` supports `--wait` (prints a dot every 0.25 seconds while waiting), `--long` and `--format json|tabular`, with cloud-init's exit codes: 0 when done, 1 when a module failed, 2 when the boot only logged warnings. It reads `/run/cloud-init/status.json` and `/run/cloud-init/result.json`. Logs are in `/var/log/cloud-init.log` and `/var/log/cloud-init-output.log`, which has the output of every command.

`cloud-init clean` removes the state in `/var/lib/cloud`, so the next boot runs every module again, as an image build does before its snapshot. It keeps the seed unless `--seed` (`-s`) is given; `--logs` (`-l`) removes the logs, and `--machine-id` resets `/etc/machine-id`. Other arguments exit 2.

Differences from cloud-init:

- No schema validation. Real cloud-init reports some values the machine accepts as schema warnings: for example `ssh_genkeytypes: []`, which the agent's DigitalOcean adapter sends, makes `cloud-init status` exit 2 on Ubuntu 24.04, and 0 here.
- `packages` skips installed packages without refreshing the package index; cloud-init always runs `apt-get update` before it installs.
- Keys go to root only. There is no default `ubuntu` user and no `disable_root` handling.
- The hostname comes from Docker; `set_hostname` does not run.

### Units

`systemctl` supports `daemon-reload`, `enable [--now]`, `disable [--now]`, `start`, `stop`, `restart`, `status` and `is-active [--quiet]` for units in `/etc/systemd/system/*.service`. These directives take effect:

| Section | Directives |
| --- | --- |
| `[Unit]` | `Description`. Ordering and dependency directives (`After=`, `Wants=`, `Requires=` and similar) are accepted and have no effect: units start when `systemctl` or the boot asks. |
| `[Service]` | `Type` (`simple`, `exec`, `oneshot`, `notify`), `ExecStart`, `ExecStartPre` (a `-` prefix ignores a failure), `Environment`, `EnvironmentFile` (`-` for an optional file), `WorkingDirectory`, `User`, `Group`, `StateDirectory` and `StateDirectoryMode` (created under `/var/lib` before each start, owned by `User` and `Group`, with `STATE_DIRECTORY` set), `AmbientCapabilities` (capability names such as `CAP_NET_BIND_SERVICE`), `Restart` (`no`, `always`, `on-failure`), `RestartSec` (the delay doubles after each quick failure, up to 30 seconds), `RemainAfterExit` |
| `[Install]` | `WantedBy` |

Other directives are logged as warnings in the unit's journal. `Type=forking`, timers, sockets and units in `/lib/systemd/system` are not supported, so services that packages install at boot are not started. `journalctl -u UNIT [-n N] [-f] [--no-pager]` prints a unit's log from `/var/log/units/<unit>.log`, which keeps earlier boots.

### Extra addresses

A cloud points a reserved IP at a machine with `docker exec -i <machine> stagely-vm-init addresses`, which reads `{"addresses": [...]}`: the machine's whole list of extra IPv4 addresses. The machine adds each new one as a `/32` on the interface that holds its public address, announces it with a gratuitous ARP so neighbors stop sending to an old MAC address, removes the ones no longer listed, and keeps the list for later boots. Other containers on the public network then reach the machine at the address. A new machine, including one booted from a snapshot, starts with no extra addresses.

### Firewall

A cloud pushes a firewall to a running machine with `docker exec -i <machine> stagely-vm-init firewall`, which reads JSON rules, applies them and keeps them for later boots. The rules sit in the `mangle` table's `PREROUTING` and `POSTROUTING` chains for the machine's outside interfaces (`eth*`), so they also cover ports that Docker inside the machine publishes, which never reach `INPUT`. Established connections, loopback traffic and traffic between containers inside the machine always pass; every other new connection that no rule allows is dropped. Rules are IPv4 only. DNS through Docker's embedded resolver (`127.0.0.11`) works whatever the rules say.

### Instance metadata

Every cloud serves a VM's metadata, including its user data, at `169.254.169.254`, from outside the VM. A machine does the same: the cloud gives it a set of documents by path, and the `stagely-metadata` unit serves them on `http://169.254.169.254/`. The server runs in its own network namespace, joined to the machine by a veth pair (`md0` in the machine, with the address `169.254.0.1`), so the machine routes requests to it as to a neighbor. Requests from containers inside the machine therefore cross Docker's `FORWARD` chain, as on a cloud VM, and rules in `DOCKER-USER` apply to them. A path that ends in `/` lists the names under it, one per line, with `/` after each directory. Other paths answer 404, and methods other than `GET` and `HEAD` answer 405. The documents are fixed when the machine is created.

### Checks

```sh
go test ./internal/machines ./vm/...                 # no Docker
go test -tags simdocker ./internal/machines          # needs Docker
```

The `simdocker` tests boot machines from seed data, check that the firewall admits only the allowed peer and survives a restart, check that an extra address reaches the machine across a restart, run a unit as an unprivileged user on port 80 with its state directory, check that the machine and its containers read the instance metadata, check that a machine trusts the `-machine-ca` certificates, boot a machine from a snapshot, and delete machines. They remove their containers and networks.

## DigitalOcean

The DigitalOcean simulation serves the DigitalOcean API v2. Each droplet is a [machine](#machines), and its firewalls are enforced inside the machine. The container registry is a CNCF Distribution registry (`registry:2`) that you run; the simulation hands out its endpoint and basic-auth credentials.

```sh
go run ./cmd/stagely-sim -digitalocean -do-publish tag=stagely-gateway:443=8443,80=8081
```

Point clients at `http://localhost:9300` with the token `dop_v1_stagely_sim`: godo with `godo.SetBaseURL`, Terraform with `api_endpoint = "http://localhost:9300"` (no path; the provider adds `/v2/`), the agent with `-do-api-url`, and `stagely setup digitalocean` with `-do-api-url`, which it passes to the module and the agent. State is kept in `<data-dir>/digitalocean/state.json`.

At start, the simulation matches stored droplets with the machines Docker has: a droplet takes its status and addresses from its machine, a droplet whose machine is gone is removed with a warning, an action that was running when the simulation stopped is marked `errored`, and a machine of this account that no droplet owns is deleted. Machines carry the labels `stagely-sim.cloud=digitalocean`, `stagely-sim.account=<account uuid>` and `stagely-sim.resource=<droplet id>`, and are named `stagely-sim-do-<account>-<droplet id>`.

### Flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `-digitalocean` | off | Run the DigitalOcean simulation |
| `-do-addr` | `:9300` | Listen address of the API |
| `-do-token` | `dop_v1_stagely_sim` | Token that the API accepts. Repeatable. All tokens act on one account. |
| `-do-regions` | `nyc1,nyc3,sfo3,ams3,fra1,lon1,sgp1,tor1,blr1,syd1` | Region slugs. Each gets a default VPC named `default-<region>`. |
| `-do-network` | `stagely-sim` | Docker network for private addresses. Public addresses use `<network>-public`. |
| `-do-publish` | none | Publish ports of matching droplets on `127.0.0.1` of the Docker host, as `tag=<tag>:<port>=<host port>,...` or `name=<droplet name>:...`. Repeatable. |
| `-do-registry-endpoint` | `localhost:5000` | `host:port` of the registry that Docker credentials name |
| `-do-registry-api` | `http://<registry endpoint>` | Base URL the simulation uses to list repositories |
| `-do-registry-username`, `-do-registry-password` | `stagely`, `stagely` | Basic-auth credentials of the registry |

A publish rule gives its host ports to the first droplet that matches it when that droplet is created, because a host port can be bound only once. A later matching droplet gets no published ports while the first exists, and the simulation logs a warning. Traffic through a published port reaches the machine from the Docker network's gateway address, so a firewall on that droplet must allow it, for example with `0.0.0.0/0`.

### Coverage

| Endpoint | Notes |
| --- | --- |
| `GET /v2/account` | Droplet limit 25 |
| `GET /v2/regions`, `GET /v2/sizes` | The `-do-regions`, and the Basic sizes from `s-1vcpu-512mb-10gb` to `s-8vcpu-16gb` with DigitalOcean's prices |
| `GET /v2/images`, `GET`/`DELETE /v2/images/{id}` | The distribution image `ubuntu-24-04-x64` (by slug or id) and snapshots. Filters: `type=distribution\|application`, `private=true`, `tag_name`. |
| `GET /v2/snapshots`, `GET`/`DELETE /v2/snapshots/{id}`, `GET /v2/droplets/{id}/snapshots` | Droplet snapshots; `resource_type=volume` lists none |
| `GET`/`POST /v2/account/keys`, `GET`/`PUT`/`DELETE /v2/account/keys/{id or fingerprint}` | The MD5 fingerprint is computed from the key. A malformed or duplicate key is a 422. |
| `POST /v2/droplets` | `name` or up to 10 `names`, `region` (default the first region), `size`, `image` (slug or id), `ssh_keys` (ids or fingerprints), `tags` (created when missing), `user_data` (up to 64 KiB), `vpc_uuid` (default the region's default VPC), `backups` and `monitoring` (features only). Answers 202 with status `new` and a `create` action link. |
| `GET /v2/droplets` | Filters `tag_name` or `name` (case-insensitive); `type=gpus` lists none |
| `GET`/`DELETE /v2/droplets/{id}`, `DELETE /v2/droplets?tag_name=` | Delete removes the machine first, so `GET` answers 404 only when the machine is gone. |
| `POST /v2/droplets/{id}/actions` | `power_off` (hard), `shutdown` (graceful), `power_on`, `reboot`, `power_cycle`, `snapshot`, `rename`, `resize` (droplet off; size only), `enable_backups`, `disable_backups` |
| `POST /v2/droplets/actions?tag_name=` | `power_cycle`, `power_on`, `power_off`, `shutdown`, `snapshot`, `enable_backups`, `disable_backups` |
| `GET /v2/actions`, `GET /v2/actions/{id}`, `GET /v2/droplets/{id}/actions[/{id}]` | Newest first. Actions stay `in-progress` while their Docker work runs. |
| `POST`/`GET /v2/tags`, `GET`/`DELETE /v2/tags/{name}`, `POST`/`DELETE /v2/tags/{name}/resources` | Create returns an existing tag in its first spelling. Droplets and images can be tagged. |
| `POST`/`GET /v2/vpcs`, `GET`/`PUT`/`PATCH`/`DELETE /v2/vpcs/{id}`, `GET /v2/vpcs/{id}/members` | `ip_range` must be an RFC 1918 block from /16 to /28 that overlaps no other VPC; without it a free /20 is chosen. Deleting a default VPC or a VPC with members is a 403. `default: true` moves the region's default. |
| `POST`/`GET /v2/firewalls`, `GET`/`PUT`/`DELETE /v2/firewalls/{id}`, `POST`/`DELETE /v2/firewalls/{id}/droplets`, `/tags`, `/rules` | `tcp`, `udp` and `icmp` allow rules with `addresses`, `droplet_ids` and `tags`. Rules are echoed as sent, with `action: allow`. At most 50 rules, 10 droplets and 5 tags; tags must exist. |
| `GET`/`POST`/`DELETE /v2/registry`, `GET`/`DELETE /v2/registries/{name}`, `POST /v2/registries`, `GET /v2/registries` | One registry per account. Names match `^[a-z0-9-]{1,63}$`; tiers `starter`, `basic`, `professional`; regions `nyc3`, `sfo3`, `ams3`, `sgp1`, `fra1`. |
| `GET`/`POST /v2/registry/subscription` and `/v2/registries/subscription`, `GET /v2/registry/options` and `/v2/registries/options` | |
| `GET /v2/registry/docker-credentials`, `GET /v2/registries/{name}/docker-credentials` | `{"auths": {"<registry endpoint>": {"auth": "<base64 user:password>"}}}`. `read_write` and `expiry_seconds` are validated. |
| `GET /v2/registry/{name}/repositoriesV2` and `/repositories`, `GET /v2/registries/{name}/repositoriesV2` | Lists `<registry name>/...` repositories from the registry's catalog API, with tag and manifest counts |
| `GET`/`POST /v2/reserved_ips`, `GET`/`DELETE /v2/reserved_ips/{ip}` | Create takes `region` or `droplet_id`; with `droplet_id` it answers with an `assign` action link. Addresses come from the middle of the public network upward. Deleting an assigned address removes it from its droplet. A droplet's deletion releases its addresses. |
| `POST`/`GET /v2/reserved_ips/{ip}/actions`, `GET /v2/reserved_ips/{ip}/actions/{id}` | `assign` (moves the address when it points at another droplet) and `unassign`, as `assign_ip` and `unassign_ip` actions with `resource_type: reserved_ip` and the address as a number in `resource_id`. The address's machine answers at it once the action completes. The droplet must be in the address's region; an address with a running action is `locked`, and another action or a delete gets a 422. |
| `GET`/`POST /v2/domains`, `GET`/`DELETE /v2/domains/{name}` | Create takes `name` and an optional `ip_address`. It adds the zone's SOA record, `NS` records for `ns1`, `ns2` and `ns3.digitalocean.com`, and an `A` (or `AAAA`) record for `@` with the address. `ttl` is 1800, and `zone_file` lists the records. A taken name is a 422. Deleting a domain deletes its records. |
| `GET`/`POST /v2/domains/{name}/records`, `GET`/`PUT`/`PATCH`/`DELETE /v2/domains/{name}/records/{id}` | Types `A`, `AAAA`, `CAA`, `CNAME`, `MX`, `NS`, `SOA`, `SRV` and `TXT`. Names are relative, with `@` for the apex; a name that ends with `.<domain>.` is stored relative. `ttl` defaults to 1800 and is at least 30. `A` and `AAAA` data must be an address of that family, `SRV` needs `port`, and `CAA` needs `tag` `issue`, `issuewild` or `iodef`; other values are a 422. Fields a type does not use are `null`. The list filters by `type` and by fully qualified `name`. PUT and PATCH change only the fields the body names. |
| Metadata service, inside each droplet: `GET http://169.254.169.254/metadata/v1.json` and `/metadata/v1/` | `v1.json` has `droplet_id`, `hostname`, `user_data`, `public_keys`, `region`, `interfaces` (`private` and `public`, each with `ipv4.ip_address` and `type`), `tags` and `features`. The tree has `id`, `hostname`, `user-data`, `public-keys`, `region`, `interfaces/{private,public}/0/ipv4/address` and `type`, and `tags/`. |

Every request behaves as follows:

- **Authentication.** `Authorization: Bearer <token>`. A missing or unknown token gets a 401 `{"id": "unauthorized", "message": "Unable to authenticate you.", "request_id": ...}`.
- **Errors.** `{"id", "message", "request_id"}` with DigitalOcean's ids: `bad_request` (400, malformed JSON), `unauthorized`, `forbidden` (403), `not_found`, `conflict` (409), `unprocessable_entity` (422, validation and pending events), `too_many_requests` (429) and `server_error` (500). Every response has an `X-Request-Id` header.
- **Rate limits.** Per token, in a sliding window: 5,000 requests an hour and 250 a minute, with `ratelimit-limit`, `ratelimit-remaining` and `ratelimit-reset` on every response and `retry-after` on a 429.
- **Pagination.** `page` and `per_page` (default 20, at most 200). Lists have `links.pages` with absolute `first`, `prev`, `next` and `last` URLs where they apply, and `meta.total`.
- **Actions.** A droplet is `locked` while one of its actions runs, from `create` through deletion. An action or a delete on a locked droplet gets a 422 (`Droplet already has a pending event.`), as in DigitalOcean. A droplet becomes `active` when its machine starts, before cloud-init finishes, as on DigitalOcean.
- **Firewalls.** Each droplet's machine enforces the union of the firewalls that name it or one of its tags. Droplet and tag sources resolve to the droplets' private and public addresses, so a firewall follows droplets as they come and go. A new droplet boots with its firewall in place. A firewall shows `status: waiting` with `pending_changes` until every running droplet it applies to has its rules, then `succeeded`. Failed pushes are retried every 5 seconds.
- **Snapshots.** A snapshot commits the machine with `docker commit` into the image `stagely-sim-do-snapshot:<account>-<id>`, which new droplets in the same region can boot. Deleting the snapshot removes the image.

### Differences from DigitalOcean

- All tokens act on one account, and the account's limits are fixed.
- Sizes are labels: machines are not limited to the size's CPUs, memory or disk. All regions share one Docker host.
- VPCs do not isolate droplets. Every private address comes from the `-do-network` network, whatever the VPC's `ip_range` says.
- No IPv6, volumes, load balancers, Kubernetes, floating IP endpoints (the old name of reserved IPs), projects, droplet agent, deny firewall rules, or `rebuild`, `restore`, `password_reset`, `change_kernel` and `enable_ipv6` actions. Requests for them get a 422 or a 404. `public_networking: false` is a 422: every droplet has a public address. Backups and monitoring exist only as droplet features.
- A snapshot does not include `/var/lib/docker`, which is a volume, so images pulled inside the droplet are not in it.
- Docker credentials are the registry's static basic-auth credentials: `read_write` and `expiry_seconds` have no effect, because the registry cannot enforce them. The repositories list has no `latest_manifest` or `latest_tag`.
- A tag added to a droplet after creation does not publish ports.
- Domains and records are only stored: no name server answers for them. DigitalOcean also refuses a domain it cannot delegate; the simulation takes any valid name.
- The metadata service is fixed when the droplet is created: later tag changes do not show. It has no `vendor-data`, `auth_key`, `dns`, reserved IP, netmask, gateway or MAC address.
- With the Terraform provider: `digitalocean_container_registry` computes `endpoint` and `server_url` from `registry.digitalocean.com` itself. Destroying `digitalocean_container_registry_docker_credentials` makes the provider post to `https://cloud.digitalocean.com/v1/oauth/revoke`, a URL it does not take from `api_endpoint`; without internet access the provider crashes there. The simulated credentials name the local registry, so the token it posts is empty.

### Checks

```sh
go test ./internal/digitalocean                                # no Docker
mise exec -- sim/scripts/check-digitalocean.sh agent           # needs Docker and Go
mise exec -- sim/scripts/check-digitalocean.sh terraform       # needs Docker, Go and OpenTofu
mise exec -- sim/scripts/check-digitalocean.sh image           # needs Docker and Go
```

Run the scripts from the repository root. Each starts its own simulation with a temporary data directory, its own Docker networks and port 9390 (`STAGELY_SIM_CHECK_PORT` changes it), and removes all of it when it ends.

- `agent` cross-compiles the `simdocker` test of the agent's adapter (`internal/agent/provider/digitalocean/sim_test.go`) and runs it in a container on both simulation networks, which reaches the simulation through `host-gateway`. The unchanged adapter registers its SSH key, creates a stagelet and a neighbor droplet with its tag, connects over SSH with the host keys it put in the user data, and waits for `cloud-init status --wait` and `docker compose version`. The test puts a cloud firewall on the tag that lets in TCP only from its own address, as setup's module lets in only the agent and the gateway, and waits until the firewall reports `succeeded`. On the stagelet it builds and starts a Compose project that pulls `busybox` from Docker Hub and fetches a page from it on the droplet's address. It checks that the neighbor can connect out to the test but not to the stagelet's published port, and that a container can't reach the metadata service. The adapter deletes both droplets, waiting until `GET` answers 404. The script then checks that no machine is left.
- `terraform` applies `internal/digitalocean/testdata/terraform` with OpenTofu and `digitalocean/digitalocean` 2.102.0, the version that `terraform/digitalocean` pins: a VPC, an SSH key, a tag, two droplets with the tag and user data, a firewall on the tag, a reserved IP assigned to a droplet, a container registry and Docker credentials. It checks that two machines exist, that `terraform plan -detailed-exitcode` finds no changes, and that no machine is left after `terraform destroy`. It downloads the provider and needs internet access for the revoke call described above.
- `image` builds the golden image in `images/digitalocean` with Packer 1.16.1 and its `digitalocean` plugin, in a container on both simulation networks: a build droplet, the provisioners, a snapshot, and the build droplet's deletion. It checks that no machine is left, then runs the `agent` check with `STAGELY_SIM_DO_GOLDEN_IMAGE` set to the snapshot, so the adapter's droplets boot from it.

### Sources

- DigitalOcean API reference and its OpenAPI source: [API reference](https://docs.digitalocean.com/reference/api/reference/), [digitalocean/openapi](https://github.com/digitalocean/openapi) (resource schemas, examples, pagination, rate limits and error shapes in `specification/`)
- Product limits: [droplets](https://docs.digitalocean.com/products/droplets/details/limits/), [VPC](https://docs.digitalocean.com/products/networking/vpc/details/limits/), [firewalls](https://docs.digitalocean.com/products/networking/firewalls/details/limits/), [droplet pricing](https://www.digitalocean.com/pricing/droplets)
- [godo](https://github.com/digitalocean/godo) v1.216.0: request and response types, `CheckResponse` error handling, rate-limit headers, `links` pagination
- [terraform-provider-digitalocean](https://github.com/digitalocean/terraform-provider-digitalocean) v2.105.0: the calls each resource makes and the fields it reads, in `digitalocean/droplet`, `sshkey`, `vpc`, `firewall`, `tag` and `registry`, and `api_endpoint` in `digitalocean/config`
- cloud-init 26.1 ([canonical/cloud-init](https://github.com/canonical/cloud-init), [docs](https://docs.cloud-init.io/)): `cloudinit/cmd/status.py` and `cloudinit/cmd/main.py` for `cloud-init status` and the status files, `DataSourceNoCloud.py`, `config/cloud.cfg.tmpl` for module order and frequency, and `cc_ssh.py`, `cc_write_files.py`, `cc_package_update_upgrade_install.py`, `cc_bootcmd.py`, `cc_runcmd.py`, `cc_scripts_user.py`, `cc_final_message.py`
- systemd: [systemd.service](https://www.freedesktop.org/software/systemd/man/latest/systemd.service.html), [systemd.exec](https://www.freedesktop.org/software/systemd/man/latest/systemd.exec.html), [sd_notify](https://www.freedesktop.org/software/systemd/man/latest/sd_notify.html)
- Docker in Docker on cgroup v2: the cgroup nesting step of [moby's `hack/dind`](https://github.com/moby/moby/blob/master/hack/dind)

## AWS

The AWS simulation serves the EC2 Query API, STS `GetCallerIdentity` and `GetFederationToken`, the ECR API and the Docker token service of the account's registries, all on one listener. Each instance is a [machine](#machines), and its security groups are enforced inside the machine. The image registry is a CNCF Distribution registry (`registry:2`) that you run under the host name `<account>.dkr.ecr.<region>.amazonaws.com` with the simulation as its token service.

```sh
go run ./cmd/stagely-sim -aws -aws-account-id 123456789012
```

Point clients at `http://localhost:9310` with the access key `AKIASTAGELYSIMULATE` and the secret `stagely-sim-secret-access-key`:

- Terraform: `endpoints { ec2 = "http://localhost:9310", ecr = "http://localhost:9310", sts = "http://localhost:9310" }` in the `aws` provider. Keep credential validation on: the simulation answers STS.
- Packer: `AWS_ENDPOINT_URL=http://localhost:9310` in the environment. The plugin's `custom_endpoint_ec2` is not enough: the client that tags the AMI ignores it.
- The agent: `-aws-ec2-url`, `-aws-ecr-url` and `-aws-sts-url`; `stagely setup aws`: `-aws-endpoint`, which it passes to the module and the agent.
- The AWS SDKs and the AWS CLI: `AWS_ENDPOINT_URL`. curl: `--aws-sigv4 aws:amz:<region>:<service> --user <key>:<secret>`.

The listener routes each request by the service in its signature's credential scope: `ec2`, `sts` or `ecr`, and serves the registry's token service at `GET /v2/token`. State is kept in `<data-dir>/aws/state.json`.

Run the registry with these settings, and give it a certificate for its host name that the machines trust (`-machine-ca`):

```sh
REGISTRY_AUTH=token
REGISTRY_AUTH_TOKEN_REALM=http://<simulation address>/v2/token
REGISTRY_AUTH_TOKEN_SERVICE=<account>.dkr.ecr.<region>.amazonaws.com
REGISTRY_AUTH_TOKEN_ISSUER=stagely-sim
REGISTRY_AUTH_TOKEN_ROOTCERTBUNDLE=<data-dir>/aws/registry-token.pem
```

The token service signs Docker in with the user `AWS` and the password of an ECR authorization token that has not expired. On a repository of the registry in the token's region, it grants `pull` when the token's caller may call `ecr:BatchGetImage` and `ecr:GetDownloadUrlForLayer` on the repository's ARN, and `push` when it may also call `ecr:BatchCheckLayerAvailability`, `ecr:InitiateLayerUpload`, `ecr:UploadLayerPart`, `ecr:CompleteLayerUpload` and `ecr:PutImage`. The IAM user may do both everywhere; a federated session may do what its session policy allows. The tokens are RS256 JWTs with the signing certificate in the `x5c` header; the simulation writes that certificate to `registry-token.pem` when it starts.

At start, the simulation matches stored instances with the machines Docker has: an instance takes its state and addresses from its machine, an instance whose machine is gone is terminated with `Server.InternalError`, a termination that was running is finished, an AMI that was being taken fails, and a machine of this account that no instance owns is deleted. Machines carry the labels `stagely-sim.cloud=aws`, `stagely-sim.account=<account id>` and `stagely-sim.resource=<instance id>`, and are named `stagely-sim-aws-<account>-<instance id>`.

### Flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `-aws` | off | Run the AWS simulation |
| `-aws-addr` | `127.0.0.1:9310` | Listen address of the EC2, STS and ECR endpoints |
| `-aws-access-key-id`, `-aws-secret-access-key` | `AKIASTAGELYSIMULATE`, `stagely-sim-secret-access-key` | The one access key that requests must be signed with |
| `-aws-account-id` | random, kept in the state | 12-digit account id. Fix it when a registry must answer under the account's host name. |
| `-aws-regions` | `us-east-1,us-east-2,us-west-2,eu-west-1,eu-central-1` | Region names. Each gets the zones `a`, `b` and `c`, a default VPC, and the stock Ubuntu AMI. |
| `-aws-network` | `stagely-sim-aws` | Docker network for private addresses. Public addresses use `<network>-public`. |

### Coverage

EC2 actions:

| Actions | Notes |
| --- | --- |
| `RunInstances` | `ImageId`, `InstanceType`, `MinCount`/`MaxCount` (at most 32 instances in the account), `KeyName`, `SubnetId` (default: the zone's default subnet), `SecurityGroupId.N` or `SecurityGroup.N` (default: the VPC's default group), `UserData` (base64, at most 16 KiB decoded), `TagSpecification.N` for `instance`, `volume` and `network-interface`, `Placement.AvailabilityZone`, `ClientToken`, and the options EC2 reports back: `MetadataOptions`, `CreditSpecification`, `DisableApiTermination`, `DisableApiStop`, `InstanceInitiatedShutdownBehavior`, `EbsOptimized`, `Monitoring`, a root `BlockDeviceMapping` with a size and type. An instance is `pending` until its machine runs, then `running`. It gets a public address when its subnet maps public addresses on launch. |
| `DescribeInstances` | `InstanceId.N` and the filters `instance-id`, `instance-state-name`, `instance-state-code`, `tag:<key>`, `tag-key`, `tag-value`, `vpc-id`, `subnet-id`, `image-id`, `instance-type`, `key-name`, `availability-zone`, `reservation-id`, `private-ip-address`, `ip-address`, `client-token`, `instance.group-id`, `instance.group-name`, `network-interface.network-interface-id`, `owner-id`. A terminated instance is listed for an hour, then unknown. |
| `TerminateInstances`, `StopInstances`, `StartInstances` | Terminate deletes the machine first: the instance is `shutting-down` until Docker confirms the machine is gone, then `terminated`. Stop (`Force` for a hard stop) keeps the machine and its private address. |
| `DescribeInstanceAttribute`, `ModifyInstanceAttribute`, `DescribeInstanceCreditSpecifications`, `GetInstanceUefiData` | The attributes Terraform reads and sets: `userData`, `instanceType`, `disableApiTermination`, `disableApiStop`, `instanceInitiatedShutdownBehavior`, `rootDeviceName`, `sourceDestCheck`, `ebsOptimized`, `enaSupport`, `groupSet`. Instances boot in legacy BIOS mode, so they have no UEFI data. |
| `DescribeNetworkInterfaces` | The primary interface of each live instance, the only one an instance has |
| `ImportKeyPair`, `CreateKeyPair`, `DescribeKeyPairs`, `DeleteKeyPair` | RSA and ED25519 public keys; fingerprints as EC2 computes them (MD5 of the key blob for RSA, base64 SHA-256 for ED25519). `CreateKeyPair` makes RSA keys only and returns the private key in PEM. |
| `CreateVpc`, `DescribeVpcs`, `DeleteVpc`, `DescribeVpcAttribute`, `ModifyVpcAttribute` | A new VPC gets a main route table with its local route, a default security group and a default network ACL. Delete refuses a VPC with subnets, an attached gateway, groups or route tables of its own. |
| `CreateSubnet`, `DescribeSubnets`, `DeleteSubnet`, `ModifySubnetAttribute` | The subnet must fit its VPC and overlap no other subnet. `MapPublicIpOnLaunch` is the only attribute that can be turned on. |
| `CreateInternetGateway`, `DescribeInternetGateways`, `AttachInternetGateway`, `DetachInternetGateway`, `DeleteInternetGateway` | One gateway per VPC |
| `CreateRouteTable`, `DescribeRouteTables`, `DeleteRouteTable`, `CreateRoute`, `ReplaceRoute`, `DeleteRoute`, `AssociateRouteTable`, `DisassociateRouteTable` | Routes to an internet gateway of the same VPC; `ClientToken` makes create idempotent |
| `DescribeNetworkAcls` | Every VPC's default ACL, which allows all traffic |
| `CreateSecurityGroup`, `DescribeSecurityGroups`, `DeleteSecurityGroup`, `AuthorizeSecurityGroupIngress`/`Egress`, `RevokeSecurityGroupIngress`/`Egress`, `DescribeSecurityGroupRules` | `IpPermissions.N` with `tcp`, `udp`, `icmp` or `-1`, port ranges, `IpRanges`, `Ipv6Ranges` (stored; machines have no IPv6) and `Groups`. A new group allows all outbound traffic. Rules with the same protocol and ports are reported as one `IpPermission`, as in EC2. Delete refuses a group that an instance uses or another group's rule names. |
| `AllocateAddress`, `DescribeAddresses`, `DescribeAddressesAttribute`, `AssociateAddress`, `DisassociateAddress`, `ReleaseAddress` | Elastic IPs come from the middle of the public network upward. An associated address replaces the instance's public address and reaches its machine as an extra address. `AllowReassociation` moves an address; release refuses an associated one. |
| `CreateImage`, `DescribeImages`, `DeregisterImage`, `DescribeSnapshots`, `DeleteSnapshot`, `DescribeVolumes` | See below |
| `CreateTags`, `DeleteTags`, `DescribeTags` | Every resource type above |
| `DescribeRegions`, `DescribeAvailabilityZones`, `DescribeInstanceTypes`, `DescribeInstanceTypeOfferings`, `DescribeAccountAttributes` | The `-aws-regions`, three zones each, and the instance types `t3.nano` to `t3.2xlarge`, `m7i.large`, `m7i.xlarge` and `c7i.large`, offered in every zone |

STS answers `GetCallerIdentity` with the account id and the IAM user `stagely-sim`, or the federated user of a session. `GetFederationToken` gives the IAM user temporary credentials (`ASIA...` access key, secret and session token) of a federated user with `Name` (2 to 32 characters of `[\w+=,.@-]`), for `DurationSeconds` (900 to 129600, by default 43200), whose permissions are the inline session `Policy` (at most 2048 characters); without a policy, the session may do nothing. Session credentials get `AccessDenied` from it. ECR answers `GetAuthorizationToken`, `CreateRepository`, `DescribeRepositories`, `DeleteRepository`, `CreateRepositoryCreationTemplate`, `DescribeRepositoryCreationTemplates`, `UpdateRepositoryCreationTemplate` and `DeleteRepositoryCreationTemplate`.

Every request behaves as follows:

- **Signatures.** Every request must carry a Signature Version 4 `Authorization` header for the configured access key, or for a session's access key with its session token in `X-Amz-Security-Token`, with `host` signed and `X-Amz-Date` within 15 minutes of the simulation's clock, scoped to a configured region (STS also takes `us-east-1`). Each service reports a refusal in its own format: EC2 `AuthFailure`, `SignatureDoesNotMatch`, `IncompleteSignature`, `MissingAuthenticationToken` or `RequestExpired`; STS `InvalidClientTokenId`, `ExpiredToken` or `SignatureDoesNotMatch`; ECR `UnrecognizedClientException`, `ExpiredTokenException` or `InvalidSignatureException`. The simulation logs why it refused a request.
- **Session policies.** A request with session credentials may call an EC2 or ECR action only when the session policy allows it on every resource (`"Resource": "*"`); otherwise EC2 answers `UnauthorizedOperation` and ECR `AccessDeniedException`. The registry's token service checks pulls and pushes against each repository's ARN. A statement allows or denies its `Action` on its `Resource`, both a string or a list with the wildcards `*` and `?`; a deny wins. `GetFederationToken` refuses a policy with any other element, such as `Condition`, `NotAction` or `Principal`, with `MalformedPolicyDocument`.
- **Regions.** Resources belong to the region of the request's signature and are invisible from other regions.
- **Errors.** EC2 answers `<Response><Errors><Error><Code/><Message/></Error></Errors><RequestID/></Response>` with EC2's codes (`InvalidInstanceID.NotFound`, `InvalidParameterValue`, `DependencyViolation`, `IncorrectInstanceState` and the others). STS answers `<ErrorResponse>`, ECR `{"__type", "message"}`.
- **Unsupported input fails.** An unknown EC2 action is `InvalidAction`, an unknown parameter `UnknownParameter`, an unknown filter `InvalidParameterValue`, and an option the simulation cannot honor (dedicated tenancy, hibernation, instance profiles, Nitro Enclaves, encrypted volumes, extra block devices, IPv6 on subnets) `UnsupportedOperation`. An unknown ECR operation is `UnknownOperationException`, an unknown field `SerializationException`. `DryRun=true` answers `DryRunOperation`.
- **Security groups.** Each instance's machine enforces the union of its groups' rules. A rule that names a group admits the private addresses of the running instances in that group, as in a VPC, and follows instances as they come and go. Failed pushes are retried every 5 seconds.
- **Images.** The stock image is Canonical's `ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-amd64-server-20260918`, owner `099720109477`, with a different id in each region; it boots the base machine image. `CreateImage` commits the instance's machine, running or stopped, into the Docker image `stagely-sim-aws-image:<account>-<snapshot id>` behind a new snapshot; the AMI is `pending` until the commit ends. Deregistering an AMI keeps its snapshot unless `DeleteAssociatedSnapshots` is set; deleting a snapshot removes its Docker image and is refused while an AMI uses it.
- **Instance metadata.** Each machine serves `http://169.254.169.254/latest/meta-data/` (`ami-id`, `instance-id`, `instance-type`, `hostname`, `local-hostname`, `local-ipv4`, `public-ipv4`, `public-hostname`, `mac`, `reservation-id`, `security-groups`, `placement/availability-zone`, `placement/availability-zone-id`, `placement/region`, `public-keys/0/openssh-key`, `network/interfaces/macs/<mac>/...`), `/latest/user-data` and `/latest/dynamic/instance-identity/document`.
- **ECR tokens.** `GetAuthorizationToken` returns base64 `AWS:<password>` with a new password for each call, `expiresAt` 12 hours ahead and the proxy endpoint `https://<account>.dkr.ecr.<region>.amazonaws.com`. The token may do in the registry what its caller may do.

### Differences from AWS

- One account with one IAM user, whose access key may do everything, and the federated sessions it creates. No IAM API, no `AssumeRole`, no managed session policies or session tags, no presigned URLs.
- Instance types are labels: machines are not limited to their CPUs or memory, and every type is `x86_64`. All regions share one Docker host.
- Addresses come from the `-aws-network` networks, whatever the VPC's and subnet's CIDR blocks say, and VPCs do not isolate instances from each other. Network ACLs exist only as defaults and are not enforced. An instance keeps its public address when it stops and starts, where AWS gives it a new one. With an Elastic IP, EC2 reports the Elastic IP as the instance's public address, as AWS does, and the machine still answers at its first address too.
- The machine's host name is the instance id; EC2 reports the `ip-...` DNS names.
- Describe actions return every match in one page: they accept `MaxResults` and `NextToken` but never return a `nextToken`.
- The metadata service answers `GET` and `HEAD` only, so the IMDSv2 token request (`PUT /latest/api/token`) gets a 405 and clients fall back to IMDSv1; `MetadataOptions` are stored and reported, not enforced. The documents are fixed when the instance launches and list only the first public key; there are no IAM credentials, `public-keys/` listing in EC2's `0=<name>` form, or `vendor-data`.
- `CreateImage` does not reboot the instance, and an image does not include `/var/lib/docker`, which is a volume. Volumes exist only as instances' root volumes.
- The registry pushes to any repository that a token allows, while ECR refuses a push to a missing repository that no `CREATE_ON_PUSH` template matches. A token keeps working for the hour of the bearer token that the token service issued, even when its ECR password expires during that hour. `DescribeRepositories` lists only repositories made with `CreateRepository`. Repository creation templates and repository settings are stored and reported, not applied.
- No spot instances, launch templates, placement groups, IPv6, NAT gateways, VPC endpoints, peering, extra network interfaces or EBS volumes, `RebootInstances`, `CopyImage` or `ModifyImageAttribute`. Requests for them are refused.
- With packer-plugin-amazon 1.8.2: the step that tags the AMI makes its own EC2 client without `custom_endpoint_ec2`, so builds against the simulation set `AWS_ENDPOINT_URL`; STS credential validation also needs it, because the plugin has no STS endpoint option.

### Checks

```sh
go test ./internal/aws                                # no Docker
mise exec -- sim/scripts/check-aws.sh agent           # needs Docker, Go, curl and python3
mise exec -- sim/scripts/check-aws.sh terraform       # also needs OpenTofu
mise exec -- sim/scripts/check-aws.sh image           # needs Docker, Go, curl and python3
```

Run the scripts from the repository root. Each starts its own simulation with a temporary data directory, its own Docker networks, a random access key and port 9394 (`STAGELY_SIM_CHECK_PORT` changes it), and removes all of it when it ends. They keep AWS clients away from your own AWS configuration.

- `agent` cross-compiles the `simdocker` test of the agent's adapter (`internal/agent/provider/aws/sim_test.go`) and runs it in a container on both simulation networks, which reaches the simulation through `host-gateway`. The test creates a security group that lets in TCP only from its own addresses, as the stagelet group lets in only the agent and the gateway. The unchanged adapter imports its key pair, launches a stagelet and a neighbor instance of the stock AMI into the default VPC in that group, connects over SSH as root with the host keys it put in the user data, waits for `cloud-init status --wait` and `docker compose version`, builds and starts a Compose project that pulls `busybox` from Docker Hub on the stagelet, and fetches a page from it on its public address. The test then checks that the neighbor can connect out to the test but not to the stagelet's published port, at its public or private address; and that a container cannot reach the metadata service. The script runs CNCF Distribution 2.8.3 as the account's ECR registry with a certificate from the check's CA, which the machines trust through `-machine-ca`, and the simulation as its token service. The stagelet signs in with a build worker's registry credentials and pushes an image, then with a stagelet machine's credentials pulls it and is refused a push. The adapter terminates both instances, waiting until EC2 reports them `terminated`. The script then checks that no machine is left.
- `terraform` applies `terraform/aws` through `internal/aws/testdata/terraform` with OpenTofu and `hashicorp/aws` 6.66.0, the version the module pins: a VPC, an internet gateway, a subnet, a route table, three security groups, a key pair, the agent and gateway instances, an Elastic IP with its association and an ECR repository creation template. It checks that two machines exist, signs in as root with the operator key to the agent host and to the gateway's Elastic IP from the public network and reads the files their user data wrote, checks that `tofu plan -detailed-exitcode` finds no changes, and that no machine is left after `tofu destroy`. It downloads the provider.
- `image` builds the golden image in `images/aws` with Packer 1.16.1 and its `amazon` plugin 1.8.2, in a container on both simulation networks with `AWS_ENDPOINT_URL` set: the source AMI lookup, a temporary key pair and security group, a build instance, the provisioners, a stop, `CreateImage`, the AMI and snapshot tags, and the cleanup. It checks that no machine is left, then runs the `agent` check with `STAGELY_SIM_AWS_GOLDEN_IMAGE` set to the AMI, so the adapter's instance boots from it.

### Sources

- [Amazon EC2 API reference](https://docs.aws.amazon.com/AWSEC2/latest/APIReference/Welcome.html): the actions above, the [Query API request format](https://docs.aws.amazon.com/AWSEC2/latest/APIReference/Query-Requests.html), [error codes and the error response](https://docs.aws.amazon.com/AWSEC2/latest/APIReference/errors-overview.html), instance states and the [instance metadata categories](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/instancedata-data-categories.html)
- [Signature Version 4](https://docs.aws.amazon.com/IAM/latest/UserGuide/reference_sigv-create-signed-request.html) and its test suite, as botocore ships it in `tests/unit/auth/aws4_testsuite`
- [AWS STS API reference](https://docs.aws.amazon.com/STS/latest/APIReference/API_GetCallerIdentity.html), [`GetFederationToken`](https://docs.aws.amazon.com/STS/latest/APIReference/API_GetFederationToken.html) and the IAM user guide on [its permissions](https://docs.aws.amazon.com/IAM/latest/UserGuide/id_credentials_temp_control-access_getfederationtoken.html) and [policy evaluation](https://docs.aws.amazon.com/IAM/latest/UserGuide/reference_policies_evaluation-logic.html), [Amazon ECR API reference](https://docs.aws.amazon.com/AmazonECR/latest/APIReference/Welcome.html) the ECR user guide on [registry authentication](https://docs.aws.amazon.com/AmazonECR/latest/userguide/registry_auth.html), [push permissions](https://docs.aws.amazon.com/AmazonECR/latest/userguide/image-push-iam.html) and [repository creation templates](https://docs.aws.amazon.com/AmazonECR/latest/userguide/repository-creation-templates.html); CNCF Distribution's [token authentication](https://distribution.github.io/distribution/spec/auth/token/) specification for the registry's token service
- [aws-sdk-go-v2](https://github.com/aws/aws-sdk-go-v2) service/ec2 v1.332.0, service/ecr v1.65.0 and service/sts v1.51.0: the query parameter and XML element names in `serializers.go` and `deserializers.go`, and the JSON field names, and in service/sts `endpoints.go` the regional STS endpoint, signed for the region, that the SDK uses by default; feature/ec2/imds v1.18.2: the fall back to IMDSv1 when the token request gets a 403, 404 or 405
- [terraform-provider-aws](https://github.com/hashicorp/terraform-provider-aws) v6.66.0: the calls and fields of `aws_instance`, `aws_vpc`, `aws_subnet`, `aws_internet_gateway`, `aws_route_table`, `aws_security_group`, `aws_key_pair`, `aws_eip`, `aws_eip_association`, `aws_ecr_repository_creation_template` and the `aws_ami` and `aws_caller_identity` data sources in `internal/service/ec2` and `internal/service/ecr`, and `aws-sdk-go-base` v2 for credential validation
- [packer-plugin-amazon](https://github.com/hashicorp/packer-plugin-amazon) v1.8.2: the `amazon-ebs` builder's steps in `common/` and `builder/ebs/`

## Google Cloud

The Google Cloud simulation serves one project: the OAuth 2.0 token endpoint for service account keys, the Compute Engine API v1, Artifact Registry repositories, IAM service accounts and impersonation, and the Docker token service of Artifact Registry's registry hosts. Each instance is a [machine](#machines), and firewall rules are enforced inside the machine. The registry itself is a CNCF Distribution registry (`registry:2`) that you run with the simulation as its token service.

```sh
go run ./cmd/stagely-sim -gcp
```

On its first start the simulation makes the project's service account and one key, and writes the key file that Google Cloud would let you download to `<data-dir>/gcp/service-account.json`. Its `token_uri` is `-gcp-token-uri`, by default `http://<gcp-addr>/token`; a client that reaches the simulation at another address needs a copy with its own `token_uri`. State is kept in `<data-dir>/gcp/state.json`, so a restart keeps the key, the issued tokens and every resource.

Point clients at the simulation with the key file:

- the agent: `-gcp-compute-url http://localhost:9320/compute/v1/`, `-gcp-iam-credentials-url http://localhost:9320/v1/` and the key in `GOOGLE_CREDENTIALS`;
- Terraform's `hashicorp/google` provider: `compute_custom_endpoint = "http://localhost:9320/compute/v1/"`, `artifact_registry_custom_endpoint` and `iam_beta_custom_endpoint` set to `"http://localhost:9320/v1/"`, and the key in `GOOGLE_CREDENTIALS`;
- Packer's `googlecompute` plugin: `custom_endpoints = { compute = "http://localhost:9320/compute/v1/" }` and the key in `GOOGLE_APPLICATION_CREDENTIALS`;
- `stagely setup gcp`: `-gcp-compute-url`, `-gcp-artifact-registry-url`, `-gcp-iam-url` and `-gcp-iam-credentials-url`, which it passes to the module and the agent;
- `curl`: `Authorization: Bearer ya29.stagely-sim`, the `-gcp-access-token`.

At start, the simulation creates its Docker networks when they are missing and matches stored instances with the machines Docker has: an instance takes its addresses and state from its machine, an instance whose machine is gone is removed with a warning, an operation that was running when the simulation stopped ends with an error, and a machine of this project that no instance owns is deleted. Machines carry the labels `stagely-sim.cloud=gcp`, `stagely-sim.account=<account uuid>` and `stagely-sim.resource=<instance id>`, and are named `stagely-sim-gcp-<account>-<instance id>`.

### Flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `-gcp` | off | Run the Google Cloud simulation |
| `-gcp-addr` | `127.0.0.1:9320` | Listen address of all the simulated APIs |
| `-gcp-project` | `stagely-sim` | Project ID |
| `-gcp-access-token` | `ya29.stagely-sim` | Access token that the APIs accept besides the tokens the simulation issues. Repeatable. |
| `-gcp-zones` | `us-central1-a,b,c,f`, `us-east1-b,c,d`, `europe-west1-b,c,d` | Zones; their regions are the region part of each name |
| `-gcp-network` | `stagely-sim-gcp` | Docker network for internal addresses. External addresses use `<network>-public`. |
| `-gcp-token-uri` | `http://<gcp-addr>/token` | `token_uri` written into the key file |

### Coverage

| Endpoint | Notes |
| --- | --- |
| `POST /token` | The JWT bearer grant (RFC 7523): an RS256 assertion signed by the project's key, with `iss` the service account, a `scope`, `aud` ending in `/token`, and at most an hour between `iat` and `exp`. Answers an access token that lasts an hour. Errors are `invalid_grant`, `invalid_scope`, `invalid_request` and `unsupported_grant_type`, as Google's. |
| `GET /compute/v1/projects/{project}` | The project with empty common instance metadata |
| `GET .../zones[/{zone}]`, `.../regions[/{region}]` | The `-gcp-zones` and their regions |
| `GET .../zones/{zone}/machineTypes[/{name}]` | `e2-micro`, `e2-small`, `e2-medium`, `e2-standard-2`, `-4`, `-8`, `n2-standard-2`, `-4`, `-8` |
| `GET .../zones/{zone}/diskTypes[/{name}]` | `pd-standard`, `pd-balanced`, `pd-ssd` |
| `GET .../global/images[/{name}]`, `.../global/images/family/{family}` | The project's images, and the public project `ubuntu-os-cloud` with `ubuntu-2404-noble-amd64-v20260910` (family `ubuntu-2404-lts-amd64`) and `ubuntu-2404-noble-arm64-v20260910` (`ubuntu-2404-lts-arm64`). A family resolves to its newest image that is not deprecated. |
| `POST`, `DELETE .../global/images[/{name}]`, `POST .../global/images/{name}/deprecate` | An image is made from a `sourceDisk` that no instance uses. Deprecation states are stored; an `OBSOLETE` or `DELETED` image no longer boots instances. |
| `POST`, `GET .../zones/{zone}/instances`, `GET`, `DELETE .../instances/{name}` | See [Instances](#instances). List filters: `name`, `status` and `labels.KEY` with `=`, `!=`, `eq` or `ne`, joined with `AND` or spaces. |
| `GET .../zones/{zone}/disks[/{name}]`, `DELETE .../disks/{name}` | Boot disks. A disk in use is a 400 `resourceInUseByAnotherResource`. |
| `POST`, `GET .../global/networks`, `GET`, `DELETE .../networks/{name}` | Auto mode and custom mode networks. An auto mode network gets a `/20` subnetwork in each region from `10.128.0.0/9`. A new project has the network `default` with the rules `default-allow-internal`, `-ssh`, `-rdp` and `-icmp` that Google Cloud gives a new project; once deleted, it stays deleted. A network that firewall rules, instances or custom subnetworks use can't be deleted. |
| `POST`, `GET .../regions/{region}/subnetworks`, `GET`, `DELETE .../subnetworks/{name}` | `ipCidrRange` from `/8` to `/29` that overlaps no other subnetwork of the network |
| `POST`, `GET .../global/firewalls`, `GET`, `DELETE .../firewalls/{name}` | See [Firewall rules](#firewall-rules) |
| `POST`, `GET .../regions/{region}/addresses`, `GET`, `DELETE .../addresses/{name}`, `POST .../addresses/{name}/setLabels` | Static external addresses. See [Instances](#instances). |
| `GET .../zones/{zone}/operations/{name}`, `.../regions/{region}/operations/{name}`, `.../global/operations/{name}`, and `POST .../{name}/wait` on each | `wait` answers when the operation is `DONE` or after two minutes. |
| `POST`, `GET /v1/projects/{project}/locations/{location}/repositories`, `GET`, `DELETE .../repositories/{repository}`, `GET .../operations/{operation}` | Artifact Registry Docker repositories in standard mode, in a region or `us`, `europe` or `asia`. The ID comes from `repositoryId` or `repository_id`. Create and delete answer a finished long-running operation. `registryUri` is `<location>-docker.pkg.dev/<project>/<repository>`. |
| `GET`, `POST .../repositories/{repository}:getIamPolicy`, `:setIamPolicy` | The repository's IAM policy. See [Service accounts](#service-accounts). |
| `POST`, `GET`, `DELETE /v1/projects/{project}/serviceAccounts[/{email}]`, `POST .../serviceAccounts/{email}:getIamPolicy`, `:setIamPolicy` | Service accounts of the IAM API, with their IAM policies. See [Service accounts](#service-accounts). |
| `POST /v1/projects/-/serviceAccounts/{email}:generateAccessToken` | The IAM Service Account Credentials API. See [Service accounts](#service-accounts). |
| `GET /v2/token` | The Docker token service of the registry hosts. See [Artifact Registry](#artifact-registry). |
| Metadata server, inside each instance: `GET http://169.254.169.254/computeMetadata/v1/...` | `instance/id`, `name`, `hostname`, `zone`, `machine-type`, `tags`, `attributes/<key>` for each metadata item (`user-data`, `ssh-keys` and the rest), `network-interfaces/0/ip`, `network`, `access-configs/0/external-ip` and `type`, and `project/project-id` and `numeric-project-id` |

Every request behaves as follows:

- **Authentication.** `Authorization: Bearer <token>` with a token the simulation issued that has not expired, or a `-gcp-access-token`. Otherwise a 401 with `status: UNAUTHENTICATED`. Only the project's own service account and `-gcp-access-token` may call the APIs; the tokens of other service accounts get a 403 and serve only to sign in to the registry. Requests for another project are a 404 (Artifact Registry and IAM: 403), except image reads from `ubuntu-os-cloud`.
- **Errors.** `{"error": {"code", "message", "errors": [{"message", "domain", "reason"}], "status"}}` with Compute Engine's reasons: `notFound`, `alreadyExists` (409), `invalid`, `parseError`, `resourceInUseByAnotherResource`, `resourceNotReady` (400), `conditionNotMet` (412), `quotaExceeded` (403) and `authError` (401). A request that the simulation would not honor is a 400 whose message ends in `is not supported by the simulation`: an unknown instance or image field, a field the simulation ignores that holds a value, or an unsupported value. Unknown paths are a 404 with the same message.
- **Self links.** Resources link to each other with `https://www.googleapis.com/compute/v1/...` URLs, as Compute Engine does, whatever address the request used. References in requests may be full URLs, partial URLs (`projects/...`, `zones/...`, `global/...`) or, where Compute Engine allows, names.
- **Pagination.** `maxResults` (default and most 500) and `pageToken`; a list with more items answers `nextPageToken`.
- **Operations.** Inserts and deletes answer an operation. Networks, subnetworks, firewall rules, addresses and images change at once and answer it `DONE`; instance inserts and deletes answer it `RUNNING` while their Docker work runs.

### Instances

An instance insert takes one network interface and one boot disk made from an image (`initializeParams.sourceImage`, with `diskName`, `diskSizeGb` of at least the image's 10 GB, `diskType` and `labels`). The interface names a `network`, a `subnetwork` or both; with neither it joins `default`, and a custom mode network needs a subnetwork. Zero or one access config of type `ONE_TO_ONE_NAT` gives the instance an external address; its `natIP` may be a static address of the zone's region that no other instance uses. Metadata items are kept in order and limited to 256 KB each and 512 KB in all. `labels` and network `tags` are validated as Compute Engine does. `scheduling` (with Compute Engine's defaults filled in), `shieldedInstanceConfig`, `advancedMachineFeatures`, `serviceAccounts`, `canIpForward` and `deletionProtection` are stored and echoed. At most 24 instances exist at a time (`INSTANCES` quota).

The instance is `PROVISIONING` while its machine is created and `RUNNING` once it starts, before cloud-init finishes. The machine boots the image's content: the [machine](#machines) image for the public images, or the Docker image a custom image holds. On the machine, `metadata.google.internal` resolves to `169.254.169.254`. Its NoCloud seed takes `user-data` (decoded when `user-data-encoding` is `base64`) and every key of `ssh-keys`, and the machine starts with the firewall its network's rules give it. An instance with a static address answers at it once the insert finishes; the machine holds it as an [extra address](#extra-addresses).

A delete marks the instance `STOPPING`, removes the machine, and then the instance, so `GET` answers 404 only when the machine is gone. A delete while the insert runs is a 400 `resourceNotReady`; a delete that fails ends its operation with an error and leaves the instance `RUNNING`. A boot disk with `autoDelete: false` outlives its instance: the simulation commits the machine to the Docker image `stagely-sim-gcp-disk:<account>-<disk id>` first. An image made from that disk shares its Docker image, which is removed when neither a disk nor an image uses it.

### Firewall rules

Each instance's machine enforces the enabled `INGRESS` allow rules of its network that target it: rules without `targetTags`, or with one of the instance's tags. A rule's peers are its IPv4 `sourceRanges` and the internal addresses of the network's instances that have one of its `sourceTags`, as Compute Engine applies source tags only to traffic between instances of the network. An ingress rule with neither gets `sourceRanges: ["0.0.0.0/0"]`. Protocols are `tcp`, `udp`, `icmp`, `all` and their numbers; ports are ports or ranges. Everything else inbound is dropped, as the network's implied rule denies it, and all egress is allowed, as the implied egress rule allows it. Rule changes and instances coming and going reach running machines in the background; failed pushes are retried every 5 seconds.

### Service accounts

The project's own service account, whose key the simulation writes, may do everything. Other service accounts have no key: a caller gets one of their tokens with `generateAccessToken` when the account's IAM policy gives the caller `roles/iam.serviceAccountTokenCreator`. The token lasts the requested `lifetime`, an hour at most, and its account holds only the roles that IAM policies give it on resources: on Artifact Registry repositories, `roles/artifactregistry.reader` lets it pull, and `roles/artifactregistry.writer`, `repoAdmin` and `admin` let it push too. Policies take bindings of a role and members (`serviceAccount:`, `user:`, `group:`, `domain:`); a `setIamPolicy` with an `etag` that is no longer current is a 409. Deleting an account revokes its tokens.

### Artifact Registry

Run a registry for each location that you need, under the location's host name on the simulated network, such as `us-central1-docker.pkg.dev`, with a TLS certificate that machines trust (`-machine-ca`) and token authentication against the simulation:

```sh
REGISTRY_AUTH=token
REGISTRY_AUTH_TOKEN_REALM=http://<simulation address>/v2/token
REGISTRY_AUTH_TOKEN_SERVICE=us-central1-docker.pkg.dev
REGISTRY_AUTH_TOKEN_ISSUER=stagely-sim
REGISTRY_AUTH_TOKEN_ROOTCERTBUNDLE=<data-dir>/gcp/registry-token.pem
```

The token service signs Docker clients in as Artifact Registry does: the user name `oauth2accesstoken` with an access token, `_json_key` with the key file, or `_json_key_base64` with the key file in base64. On `<project>/<repository>/<image>`, when the Docker repository exists in the service's location, it grants the project's own account `pull` and `push`, and another service account what its role on the repository allows; otherwise it grants nothing. Its tokens are RS256 JWTs with the signing certificate in the `x5c` header; the simulation writes that certificate to `registry-token.pem`.

### Differences from Google Cloud

- One project. IAM is simulated only as far as [Service accounts](#service-accounts) describes: the project's own account may do everything, project-level roles, custom roles, conditions, scopes and service account keys other than the project's own are not simulated, and the IAM API serves only service accounts and their policies. The only public images are the two Ubuntu 24.04 images, and both boot the machine image on the Docker host's CPU, whatever their architecture says.
- Only service account keys get tokens, through the JWT bearer grant. Self-signed JWTs as bearer tokens, user credentials, ID tokens (`target_audience`) and domain-wide delegation (`sub`) are not simulated. The assertion's `aud` must end in `/token` rather than be Google's token URI, because clients reach the simulation at several addresses.
- Machine types are labels: machines are not limited to their CPUs or memory. All zones share one Docker host, and no quota but `INSTANCES` is enforced. There are no rate limits.
- Networks do not isolate instances. Every internal address comes from the `-gcp-network` network, whatever the subnetwork's range says, and source ranges that name a subnetwork's range match no instance. There are no routes, peering, Cloud NAT (an instance without an access config still reaches the internet through Docker), IPv6, legacy networks, secondary ranges or flow logs.
- Instances have one network interface and one boot disk made from an image. Attaching existing disks, local SSDs, GPUs, choosing the internal address, and the methods `start`, `stop`, `reset`, `setMetadata`, `setLabels`, `setTags`, `getSerialPortOutput` and the rest are not simulated. Metadata keys that only the guest agent acts on (`startup-script`, `shutdown-script` and their `-url` forms, `enable-oslogin`, `windows-startup-script-ps1`) are rejected when they hold a value, because no guest agent runs. Scheduling, shielded VM and machine feature settings are echoed, not applied, and the metadata server serves no service account tokens.
- `ssh-keys` entries give their key to root, whatever user they name: the simulated machine has no guest agent that creates users. Use `root` as the user.
- The metadata server is fixed when the instance is created. It does not require or send the `Metadata-Flavor: Google` header and has no `recursive`, `alt=json` or `wait_for_change` queries. It serves no DNS: the machine resolves names through Docker, so containers inside it do not resolve `metadata.google.internal`, which on Compute Engine they do through the metadata server's address.
- Firewall rules are allow rules. Deny rules, service account sources and targets, and rule logging are rejected; priorities have no effect; egress rules are accepted and change nothing. Rules are IPv4 only. Firewall rules and other resources cannot be updated in place: there is no `PATCH`.
- Static addresses are external and regional, and reach an instance only through its insert: access configs cannot be changed later. Internal addresses and choosing the address are not simulated.
- An image can be made only from a disk that no instance uses (no `forceCreate`), and only of what the machine's filesystem held: `/var/lib/docker` is a volume, so images pulled inside the instance are not in it.
- Artifact Registry stores no images itself: deleting a repository leaves its images in the registry that you run. Cleanup policies, remote and virtual repositories, other formats and KMS keys are rejected.
- With the Terraform provider: at start it fetches `https://openidconnect.googleapis.com/v1/userinfo`, a URL it does not take from the custom endpoints, to log who it signs in as, with the simulated access token. Without internet access the request fails, and the provider logs it and goes on.

### Checks

```sh
go test ./internal/gcp                               # no Docker
mise exec -- sim/scripts/check-gcp.sh agent          # needs Docker and Go
mise exec -- sim/scripts/check-gcp.sh terraform      # needs Docker, Go and OpenTofu
mise exec -- sim/scripts/check-gcp.sh image          # needs Docker and Go
```

Run the scripts from the repository root. Each starts its own simulation with a temporary data directory, its own Docker networks and port 9393 (`STAGELY_SIM_CHECK_PORT` changes it), and removes all of it when it ends. The simulation starts with a CA the script makes, which machines trust.

- `agent` cross-compiles the `simdocker` test of the agent's adapter (`internal/agent/provider/gcp/sim_test.go`) and runs it in a container on both simulation networks, which reaches the simulation through `host-gateway` with a key file whose `token_uri` points there. The script runs CNCF Distribution 2.8.3 as `us-central1-docker.pkg.dev` with a certificate from the CA and the simulation as its token service. The test, with the agent's key, makes what setup's module makes: a custom mode network whose only firewall rule lets the test's addresses reach the stagelet tag, a Docker repository, and push and pull service accounts that the key's account may impersonate. The unchanged adapter creates a stagelet and a neighbor instance, connects over SSH with the host keys it put in the user data, waits for `cloud-init status --wait` and `docker compose version`, builds and starts a Compose project on the stagelet that pulls `busybox` from Docker Hub, and fetches a page from it on the internal address. The neighbor can connect out but not to the stagelet's published port; the stagelet resolves `metadata.google.internal` to the metadata server, and a container on it can't connect there. The stagelet signs in to the registry with the push account's token and pushes an image, then with the pull account's token pulls it and is refused a push. The adapter lists its instances by label and deletes both, waiting until `GET` answers 404. The script then checks that no machine is left.
- `terraform` applies `terraform/google-cloud`, the module that `stagely setup gcp` applies, with OpenTofu and `hashicorp/google` 8.4.0, the version it pins: a network and subnetwork, four firewall rules, a static address, the agent and gateway hosts, an Artifact Registry repository, and the registry's push and pull service accounts with their roles. The hosts get user data with a host key, as setup writes it. The script checks that two machines exist, signs in as root to the agent's address and to the gateway's static address trusting only that host key, gets a token of each registry account with the key's account, as the agent does, checks that `tofu plan -detailed-exitcode` finds no changes, and that no machine is left after `tofu destroy`. It downloads the provider, which also calls Google's userinfo endpoint, as described above.
- `image` builds the golden image in `images/google-cloud` with Packer 1.16.1 and its `googlecompute` plugin 1.2.7, in a container on both simulation networks: a build instance whose boot disk outlives it, the provisioners, the instance's deletion, an image of the disk, and the disk's deletion. It checks that the image exists and no machine is left, then runs the `agent` check with `STAGELY_SIM_GCP_GOLDEN_IMAGE` set to the image, so the adapter's instance boots from it.

### Sources

- Compute Engine API v1: the [REST reference](https://cloud.google.com/compute/docs/reference/rest/v1) and its discovery document as [google-api-go-client](https://github.com/googleapis/google-api-go-client) v0.298.0 ships it (`compute/v1/compute-api.json`): resource fields, list `filter`, `maxResults` and `pageToken`, operations and their `wait` method, and error shapes
- [Using OAuth 2.0 for server to server applications](https://developers.google.com/identity/protocols/oauth2/service-account) and RFC 7523; [golang.org/x/oauth2](https://github.com/golang/oauth2) v0.36.0 (`google/google.go`, `jwt/jwt.go`) for the assertion and the key file fields
- Compute Engine docs: [firewall rules](https://cloud.google.com/firewall/docs/firewalls) (implied rules, source tags), [default network rules](https://cloud.google.com/vpc/docs/firewalls#more_rules_default_vpc), [auto mode ranges](https://cloud.google.com/vpc/docs/subnets#ip-ranges), [static external addresses](https://cloud.google.com/compute/docs/ip-addresses/reserve-static-external-ip-address), [metadata server](https://cloud.google.com/compute/docs/metadata/querying-metadata) and [predefined keys](https://cloud.google.com/compute/docs/metadata/predefined-metadata-keys), [SSH keys in metadata](https://cloud.google.com/compute/docs/connect/add-ssh-keys)
- Artifact Registry: the [API v1 reference](https://cloud.google.com/artifact-registry/docs/reference/rest), [roles](https://cloud.google.com/artifact-registry/docs/access-control) and [Docker authentication](https://cloud.google.com/artifact-registry/docs/docker/authentication) (`oauth2accesstoken`, `_json_key`, `_json_key_base64`); CNCF Distribution's [token authentication](https://distribution.github.io/distribution/spec/auth/token/) and [JWT](https://distribution.github.io/distribution/spec/auth/jwt/) specifications and its `registry/auth/token` code in v2.8.3
- IAM: the [IAM API v1](https://cloud.google.com/iam/docs/reference/rest) service accounts and `getIamPolicy`/`setIamPolicy`, and the [IAM Service Account Credentials API](https://cloud.google.com/iam/docs/reference/credentials/rest) `generateAccessToken` with [service account impersonation](https://cloud.google.com/iam/docs/service-account-impersonation)
- [terraform-provider-google](https://github.com/hashicorp/terraform-provider-google) v8.4.0: the calls each resource makes and the fields it reads, in `google/services/compute` (`resource_compute_instance.go`, `compute_instance_helpers.go`, `compute_operation.go`, `image.go`, `resource_compute_network.go`, `resource_compute_subnetwork.go`, `resource_compute_firewall.go`, `resource_compute_address.go`) `google/services/artifactregistry` (with `iam_artifact_registry_repository.go`) and `google/services/resourcemanager` (`resource_google_service_account.go`, `iam_service_account.go`), and credentials and custom endpoints in `google/transport/config.go`
- [packer-plugin-googlecompute](https://github.com/hashicorp/packer-plugin-googlecompute) v1.2.7: `lib/common/driver_gce.go` and the build steps in `builder/googlecompute`
- [GoogleCloudPlatform/guest-agent](https://github.com/GoogleCloudPlatform/guest-agent): `instance_setup.go` (host keys on the first boot, `set_host_keys`) and `non_windows_accounts.go` (`ssh-keys`)

## Hetzner Cloud

The Hetzner Cloud simulation serves the Hetzner Cloud API v1. Each server is a [machine](#machines): its primary IPv4 address is the machine's public address, its firewalls are enforced inside the machine, and the machine serves Hetzner's instance metadata. Hetzner has no managed container registry, so the simulation has none; run CNCF Distribution (`registry:2`) for images.

```sh
go run ./cmd/stagely-sim -hetzner
```

Point clients at `http://localhost:9400/v1` with the token `stagely-sim-hcloud-` followed by 45 zeros: hcloud-go with `hcloud.WithEndpoint`, Terraform with the provider's `endpoint` argument or `HCLOUD_ENDPOINT`, Packer with the `endpoint` option, the agent with `-hcloud-api-url`, and `stagely setup hetzner` with `-hcloud-api-url`, which it passes to the module and the agent. Every client takes the base URL with the `/v1` path. State is kept in `<data-dir>/hetzner/state.json`.

At start, the simulation matches stored servers with the machines Docker has: a server takes its status and private address from its machine, a server whose machine is gone is removed with a warning, an action that was running when the simulation stopped ends with `error`, a snapshot that was being taken is dropped, and a machine of this project that no server owns is deleted. Machines carry the labels `stagely-sim.cloud=hetzner`, `stagely-sim.account=<project uuid>` and `stagely-sim.resource=<server id>`, and are named `stagely-sim-hz-<project>-<server id>`.

### Flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `-hetzner` | off | Run the Hetzner Cloud simulation |
| `-hcloud-addr` | `127.0.0.1:9400` | Listen address of the API |
| `-hcloud-token` | `stagely-sim-hcloud-000…` | Token that the API accepts. It must be 64 characters long, as Hetzner's tokens are, because the Terraform provider refuses other lengths. Repeatable. All tokens act on one project. |
| `-hcloud-network` | `stagely-sim-hetzner` | Docker network for private addresses. Public addresses use `<network>-public`. |

The simulation has no flag that publishes machine ports on the Docker host. On macOS, reach machines from a container on the two networks, as the checks do.

### Coverage

| Endpoint | Notes |
| --- | --- |
| `GET /v1/locations`, `GET /v1/locations/{id}` | `fsn1`, `nbg1`, `hel1`, `ash`, `hil` and `sin`, with Hetzner's ids, descriptions and network zones. Filter `name`. |
| `GET /v1/server_types`, `GET /v1/server_types/{id}` | Cost-Optimized `cx23` to `cx53` (x86) and `cax11` to `cax41` (arm) in `fsn1`, `nbg1` and `hel1`; Regular Performance `cpx12` to `cpx62` in every location. Cores, memory and disk as hetzner.com lists them. Filter `name`. |
| `GET /v1/images`, `GET`/`PUT`/`DELETE /v1/images/{id}` | The system image `ubuntu-24.04`, one per architecture, and snapshots. Filters `type`, `status`, `name`, `architecture`, `label_selector`. `PUT` changes a snapshot's description and labels. A system image answers 403 to `PUT` and `DELETE`. Deleting a snapshot removes its machine image. |
| `GET`/`POST /v1/ssh_keys`, `GET`/`PUT`/`DELETE /v1/ssh_keys/{id}` | The MD5 fingerprint is computed from the key, and the key is echoed as sent. Filters `name`, `fingerprint`, `label_selector`. A duplicate name or key is a 409 `uniqueness_error`; a malformed key is a 422 `invalid_input`. |
| `POST /v1/servers` | `name` (an RFC 1123 hostname, unique), `server_type` and `image` (id or name; a system image is found by name for the server type's architecture), `location` (default the server type's first location), `ssh_keys` (ids or names), `user_data` (up to 32 KiB), `labels`, `firewalls`, `networks` (one at most), and `public_net` with `enable_ipv6: false` and an optional `ipv4` primary IP id. Without `ipv4`, a new primary IP that is deleted with the server becomes the server's address. Answers 201 with the server `initializing` and its public IPv4 address, the `create_server` action and the `start_server` next action. |
| `GET /v1/servers` | Filters `name`, `label_selector` and `status` |
| `GET`/`PUT`/`DELETE /v1/servers/{id}` | `PUT` renames or relabels. `DELETE` answers 200 with a `delete_server` action and removes the machine first, so `GET` answers 404 only when the machine is gone. The server's auto-delete primary IP goes with it; other primary IPs become unassigned. |
| `POST /v1/servers/{id}/actions/{action}` | `poweron` (`start_server`), `poweroff` (`stop_server`, hard), `shutdown` (`shutdown_server`, graceful), `reboot` (`reboot_server`), `reset` (`reset_server`), `create_image` (snapshots only), `attach_to_network` and `detach_from_network`. Any other action is a 422 `unsupported_error`. |
| `GET`/`POST /v1/primary_ips`, `GET`/`PUT`/`DELETE /v1/primary_ips/{id}` | IPv4 primary IPs in a location, with addresses from the middle of the public network upward. `assignee_type` is `unassigned` while no server has the IP. Deleting an assigned primary IP is a 422 `must_be_unassigned`. Filters `name`, `ip`, `label_selector`. |
| `GET`/`POST /v1/firewalls`, `GET`/`PUT`/`DELETE /v1/firewalls/{id}` | `tcp`, `udp` and `icmp` rules with `source_ips` (in) or `destination_ips` (out) as CIDR blocks without host bits, and `port` as a port, a range or `any` for `tcp` and `udp`. At most 50 rules and 100 blocks a rule. `apply_to` takes servers and label selectors. Rules are echoed with IPv6 blocks in canonical form and a null `port` and `description` where none was given. Deleting a firewall that applies to something is a 422 `resource_in_use`. |
| `POST /v1/firewalls/{id}/actions/{set_rules,apply_to_resources,remove_from_resources}` | `set_firewall_rules`, `apply_firewall` and `remove_firewall` actions. A server that a label selector covers can't be removed on its own (422 `firewall_managed_by_label_selector`). |
| `GET`/`POST /v1/networks`, `GET`/`PUT`/`DELETE /v1/networks/{id}`, `POST /v1/networks/{id}/actions/{add_subnet,delete_subnet}` | `ip_range` must be an RFC 1918 block of /24 or larger. Subnets are of type `cloud`; without `ip_range`, a subnet takes the first free /24. A subnet's gateway is the first address of the network. Deleting a network detaches its servers. |
| `GET /v1/actions?id=`, `GET /v1/actions/{id}` | `GET /v1/actions` needs `id`, as on Hetzner. Filter `status`; sort `id`, `command`, `status`, `started`, `finished`. |
| `GET /v1/{servers,primary_ips,firewalls,networks}/actions[/{id}]`, `GET /v1/{…}/{id}/actions[/{action_id}]` | The actions of a resource type, or of one resource. Filters `id` and `status`. |
| Metadata service, inside each server: `GET http://169.254.169.254/hetzner/v1/metadata`, `/hetzner/v1/metadata/{hostname,instance-id,public-ipv4,private-networks,availability-zone,region}` and `/hetzner/v1/userdata` | `metadata` is YAML with `availability-zone`, `hostname`, `instance-id`, `public-ipv4`, `public-keys` and `region` (the network zone). `private-networks` is the YAML list cloud-init's Hetzner datasource reads, with `ip`, `alias_ips`, `interface_num`, `mac_address`, `network_id`, `network_name`, `network`, `subnet` and `gateway`. |

Every request behaves as follows:

- **Authentication.** `Authorization: Bearer <token>`. A missing or unknown token gets a 401 `{"error": {"code": "unauthorized", "message": "unable to authenticate", "details": null}}`.
- **Errors.** `{"error": {"code", "message", "details"}}` with Hetzner's codes: `json_error` (400), `unauthorized` (401), `forbidden` and `resource_limit_exceeded` (403), `not_found` (404), `method_not_allowed` (405), `uniqueness_error` (409), `resource_unavailable` (412), `invalid_input` (422, with the field in `details.fields`), `service_error` and `unsupported_error` (422), the operation-specific 422 codes listed above, `locked` (423), `rate_limit_exceeded` (429) and `server_error` (500).
- **Rate limits.** One budget for the project: 3,600 requests, refilled by one a second, with `RateLimit-Limit`, `RateLimit-Remaining` and `RateLimit-Reset` on every response.
- **Pagination.** `page` and `per_page` (default 25, at most 50). Lists have `meta.pagination` with `page`, `per_page`, `previous_page`, `next_page`, `last_page` and `total_entries`, and a `Link` header with `prev`, `next` and `last` where they apply.
- **Sorting and labels.** `sort` takes the fields each list documents, with `:asc` or `:desc`; another field is a 422. Labels follow Hetzner's key and value rules, and the `hetzner.cloud/` prefix is reserved. `label_selector` supports `k==v`, `k=v`, `k!=v`, `k`, `!k`, `k in (…)` and `k notin (…)`, joined by commas.
- **Actions.** Actions are `running` while their Docker work runs, then `success` or `error`. While a server action runs, another action or a delete on that server gets a 423 `locked`. A server becomes `running` when its machine starts, before cloud-init finishes, as on Hetzner.
- **Firewalls.** Each server's machine enforces the union of the firewalls that apply to it directly or through a label selector. Inbound traffic needs a rule; outbound traffic is open unless a firewall has outbound rules, as on Hetzner. On a server attached to a network, all traffic to and from the private Docker network passes, because Hetzner's firewalls do not filter private networks. A new server boots with its firewall in place. A firewall action stays `running`, and the server reports the firewall as `pending`, until every server it covers has the new rules; then `success` and `applied`. Failed pushes are retried every 5 seconds.
- **Snapshots.** `create_image` commits the machine with `docker commit` into the image `stagely-sim-hz-snapshot:<project>-<id>`, which new servers can boot by the snapshot's id.

### Differences from Hetzner Cloud

- All tokens act on one project, and no project limits apply.
- Server types are labels: machines are not limited to their cores, memory or disk, and run on the Docker host's CPU whatever the architecture. Server types carry no prices. The ids of server types and system images are the simulation's own. All locations share one Docker host.
- No IPv6. `public_net.enable_ipv6` must be `false`; leaving out `public_net`, which on Hetzner gives a server IPv4 and IPv6, is a 422 `unsupported_error`, as are IPv6 primary IPs. Firewall rules echo IPv6 blocks, and machines enforce only the IPv4 ones.
- Every server has a public IPv4 address; `enable_ipv4: false` is a 422. A server's primary IP is fixed when the server is created: `assign` and `unassign` actions, and `assignee_id` on a new primary IP, are 422s.
- Networks do not isolate servers. Every private address comes from the `-hcloud-network` network, whatever the subnet's `ip_range` says, and a server keeps its private address when it leaves a network. The firewall of a server on a network lets in every address on that Docker network, including containers that are not servers, such as a check's client. A server joins one network at most; `ip` and `alias_ips` on `attach_to_network` are 422s. `mac_address` is made up. Routes, vSwitches, `change_ip_range` and `GET /v1/networks/{id}/members` are not simulated, and `delete_subnet` is refused while servers are attached to the network.
- No volumes, placement groups, ISOs, rescue mode, backups, `rebuild`, `change_type`, protection, reverse DNS (`dns_ptr` is empty), floating IPs, load balancers, certificates, DNS zones or storage boxes. `start_after_create: false` is a 422.
- `shutdown` succeeds when the machine is off; Hetzner reports success once it sent the ACPI request.
- `esp` and `gre` firewall rules are 422s. The limit of 5 firewalls a server is checked only for firewalls applied to the server directly.
- The metadata service is fixed when the server is created: networks attached later and renames do not show. It has no `network-config` or `vendor_data`, and machines boot from NoCloud seed data, not from cloud-init's Hetzner datasource. A rename does not change the machine's hostname.
- `datacenter` fields, which Hetzner removed from the API, are 422s.
- A snapshot does not include `/var/lib/docker`, which is a volume, so images pulled inside the server are not in it.
- With the Terraform provider: a `network` block inside `hcloud_server` without an `ip` plans a change on every run against the simulation. The provider's own tests always set `ip`, which the simulation refuses. Use `hcloud_server_network`, which keeps the computed address.

### Checks

```sh
go test ./internal/hetzner                                # no Docker
mise exec -- sim/scripts/check-hetzner.sh agent           # needs Docker and Go
mise exec -- sim/scripts/check-hetzner.sh terraform       # needs Docker, Go and OpenTofu
mise exec -- sim/scripts/check-hetzner.sh image           # needs Docker and Go
```

Run the scripts from the repository root. Each starts its own simulation with a temporary data directory, its own Docker networks and port 9391 (`STAGELY_SIM_CHECK_PORT` changes it), and removes all of it when it ends.

- `agent` cross-compiles the `simdocker` test of the agent's adapter (`internal/agent/provider/hetzner/sim_test.go`) and runs it in a container on both simulation networks, which reaches the simulation through `host-gateway`. As setup's module does, the test makes a network whose range is the private Docker network's, and a firewall without rules on the label, and waits for its actions. It lets only its own private address, the agent's here, connect to servers on the network. The unchanged adapter registers its SSH key, creates a stagelet and a neighbor server with its label on the network, with user data that holds the host keys and the rules for the network, connects over SSH on their private addresses, and waits for `cloud-init status --wait` and `docker compose version`. On the stagelet it builds and starts a Compose project that pulls `busybox` from Docker Hub, fetches a page from it on the server's private address, and reads the server's hostname from the metadata service. It checks that the neighbor can connect out to the test over the network but not to the stagelet's published port or SSH port, which only the stagelet's own rules refuse, and that a container on the stagelet can't connect to the test over the network or reach the metadata service. The adapter deletes both servers, waiting until `GET` answers 404. The script then checks that no machine is left.
- `terraform` applies the bootstrap module `terraform/hetzner`, then `internal/hetzner/testdata/terraform`, with OpenTofu and `hetznercloud/hcloud` 1.69.0. The module makes a network with a subnet, an SSH key, a primary IP, three firewalls, one without rules on a label selector, two servers, one of them on the primary IP, and their network attachments. The test configuration makes a network with a subnet, an SSH key, a firewall with inbound and outbound rules on a label selector, a primary IP, two labeled servers and their network attachments. For each, the script checks that two machines exist, that `tofu plan -detailed-exitcode` finds no changes, and that no machine is left after `tofu destroy`. It downloads the provider.
- `image` builds the golden image in `images/hetzner` with Packer 1.16.1 and its `hcloud` plugin, in a container on both simulation networks: a temporary SSH key, a build server, the provisioners, a shutdown, a snapshot, and the build server's and the key's deletion. It checks that no machine is left, then runs the `agent` check with `STAGELY_SIM_HCLOUD_GOLDEN_IMAGE` set to the snapshot, so the adapter's servers boot from it.

### Sources

- Hetzner Cloud API reference and its OpenAPI spec, [docs.hetzner.cloud](https://docs.hetzner.cloud/) and `https://docs.hetzner.cloud/cloud.spec.json` as of 11 October 2026 (resource schemas, errors, pagination, rate limits, labels and label selectors, actions, server metadata), and the [changelog](https://docs.hetzner.cloud/changelog) (firewall action changes, `assignee_type: unassigned`, removed datacenters)
- Hetzner Docs: [firewalls FAQ](https://docs.hetzner.com/cloud/firewalls/faq/) (default rules, private networks, limits), [primary IPs](https://docs.hetzner.com/cloud/servers/primary-ips/overview/), [networks](https://docs.hetzner.com/cloud/networks/overview/), [servers](https://docs.hetzner.com/cloud/servers/overview/); server type sizes from [Cost-Optimized](https://www.hetzner.com/cloud/cost-optimized/) and [Regular Performance](https://www.hetzner.com/cloud/regular-performance/)
- [hcloud-go](https://github.com/hetznercloud/hcloud-go) v2.48.0: `hcloud/schema` for request and response shapes, the error, retry and pagination handlers, and the action waiter
- [terraform-provider-hcloud](https://github.com/hetznercloud/terraform-provider-hcloud) v1.69.0: `endpoint` and `HCLOUD_ENDPOINT`, the 64-character token check, and the calls and fields of `hcloud_server`, `hcloud_primary_ip`, `hcloud_ssh_key`, `hcloud_firewall`, `hcloud_network`, `hcloud_network_subnet` and `hcloud_server_network`; location data from its acceptance tests
- [packer-plugin-hcloud](https://github.com/hetznercloud/packer-plugin-hcloud) v1.8.1: `builder/hcloud` (`endpoint`, the build steps and their calls)
- cloud-init 26.1: `cloudinit/sources/DataSourceHetzner.py` and `cloudinit/sources/helpers/hetzner.py` for the metadata paths and formats

## Vultr

The Vultr simulation serves the Vultr API v2. Each instance is a [machine](#machines), and its firewall group is enforced inside the machine. A container registry's images live in a CNCF Distribution registry (`registry:2`) that you run under the registry's host name, `<region>.vultrcr.com`; the simulation hands out its root user's credentials.

```sh
go run ./cmd/stagely-sim -vultr
```

Point the agent at `http://localhost:9400` with `-vultr-api-url` and the key `STAGELYSIMVULTRAPIKEY000000000000001` in `VULTR_API_KEY`. govultr takes another URL with `SetBaseURL`. The Terraform provider and the Packer plugin always call `https://api.vultr.com`, so for them the simulation must answer under that name: give it a certificate for `api.vultr.com` with `-vultr-tls-cert` and `-vultr-tls-key`, run it on a Docker network with the alias `api.vultr.com`, and run the clients on that network, trusting the certificate's CA. `sim/scripts/check-vultr.sh` and a Compose project set this up the same way. State is kept in `<data-dir>/vultr/state.json`.

At start, the simulation matches stored instances with the machines Docker has: an instance takes its power state and addresses from its machine, an instance whose machine is gone or whose deletion was interrupted is removed with a warning, a snapshot that was being taken is dropped, and a machine of this account that no instance owns is deleted. Machines carry the labels `stagely-sim.cloud=vultr`, `stagely-sim.account=<account id>` and `stagely-sim.resource=<instance id>`, and are named `stagely-sim-vultr-<account>-<instance id>`.

### Flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `-vultr` | off | Run the Vultr simulation |
| `-vultr-addr` | `127.0.0.1:9400` | Listen address of the API |
| `-vultr-api-key` | `STAGELYSIMVULTRAPIKEY000000000000001` | API key that the API accepts. Repeatable. All keys act on one account. |
| `-vultr-network` | `stagely-sim-vultr` | Docker network for private addresses. Public addresses use `<network>-public`. |
| `-vultr-tls-cert`, `-vultr-tls-key` | none | PEM certificate and key to serve the API over HTTPS with, such as one for `api.vultr.com` |
| `-vultr-registry-username`, `-vultr-registry-password` | `stagely`, `stagely` | Root user of every container registry, which the registry you run must accept |

### Coverage

| Endpoint | Notes |
| --- | --- |
| `GET /v2/account` | |
| `GET /v2/regions`, `GET /v2/plans`, `GET /v2/os` | The 33 regions and the vc2 plans that Vultr listed on 2026-10-11, every plan in every region; `type` other than `vc2` or `all` lists no plans. The only operating system is `2284`, Ubuntu 24.04 LTS x64. |
| `GET`/`POST /v2/ssh-keys`, `GET`/`PATCH`/`DELETE /v2/ssh-keys/{id}` | The key must be an OpenSSH public key and is stored as sent. |
| `POST /v2/instances` | `region`, `plan`, `os_id` (2284) or `snapshot_id` (a complete snapshot), `label`, `hostname` (default `vultr-guest`), `tags`, `sshkey_id`, `user_data` (base64), `attach_vpc` (VPCs in the region), `firewall_group_id`, `reserved_ipv4` (a free reserved IP in the region, which becomes the main address: the machine's public address, which it sends from). Answers 202 with status `pending`, power `running`, `main_ip` `0.0.0.0` and a `default_password`; the instance becomes `active` with server status `ok` when its machine starts, before cloud-init finishes. |
| `GET /v2/instances` | Filters `label`, `main_ip`, `region`, `firewall_group_id`, `hostname` and `tag` |
| `GET`/`PATCH`/`DELETE /v2/instances/{id}` | `PATCH` changes `label`, `tags` (null keeps them), `plan`, `firewall_group_id` and `attach_vpc`/`detach_vpc`, and answers 202. Delete removes the machine first: meanwhile `GET`, `PATCH` and `DELETE` answer 400 `Server is pending destruction.`, and `GET` answers 404 only once the machine is gone. A delete while the machine is still being created destroys it once it exists. |
| `GET /v2/instances/{id}/vpcs`, `/vpc2`, `/backup-schedule`, `/user-data` | `vpcs` lists the attached VPCs with the private address; `vpc2` is always empty; the Terraform provider reads both. The backup schedule is `enabled: false`; user data comes back base64 encoded. |
| `GET`/`POST /v2/vpcs`, `GET`/`PUT`/`DELETE /v2/vpcs/{id}` | `v4_subnet` and `v4_subnet_mask` together: an RFC 1918 network address with a mask from /8 to /29; without them a free /20 in `10.0.0.0/8` is chosen. `PUT` changes the description. Deleting a VPC with an attached instance, including one being destroyed, is a 400 that says the instances are `attached to this VPC network`. |
| `GET`/`POST /v2/firewalls`, `GET`/`PUT`/`DELETE /v2/firewalls/{id}` | At most 50 rules a group. Deleting a group takes it off its instances, which then accept all traffic. |
| `GET`/`POST /v2/firewalls/{id}/rules`, `GET`/`DELETE /v2/firewalls/{id}/rules/{rule}` | `ip_type` `v4` or `v6`, `protocol` `tcp`, `udp` or `icmp`, `subnet` and `subnet_size`, `port` (a port or a range such as `8000:9000`, for tcp and udp only), `notes`. Rules are echoed with `action: accept`. |
| `GET`/`POST /v2/reserved-ips`, `GET`/`PATCH`/`DELETE /v2/reserved-ips/{id}`, `POST .../attach`, `POST .../detach` | IPv4 only, from the middle of the public network upward. `attach` routes the address to an instance in its region, whose machine then answers at it; attaching it to the instance that has it already succeeds, and an address attached elsewhere must be detached first. Deleting an attached address takes it off its instance. A deleted instance releases its addresses. |
| `GET`/`POST /v2/snapshots`, `GET`/`PUT`/`DELETE /v2/snapshots/{id}` | A snapshot commits the instance's machine with `docker commit` into the image `stagely-sim-vultr-snapshot:<account>-<id>`, `pending` until done, then `complete` with its size. A failed commit removes the snapshot. Filter `description`. Deleting a snapshot removes the image. |
| `POST /v2/registry`, `GET /v2/registries`, `GET`/`PUT`/`DELETE /v2/registry/{id}`, `GET /v2/registry/plan/list` | `name` (lowercase letters and digits, unique), `region` (any region), `plan` (`start_up`, `business`, `premium`, `enterprise`, with the storage and prices of Vultr's docs), `public`. The registry is `<region>.vultrcr.com/<name>`, and its `root_user` holds the `-vultr-registry-*` credentials. |
| `OPTIONS /v2/registry/{id}/docker-credentials` | `{"auths": {"<region>.vultrcr.com": {"auth": "<base64 user:password>"}}}` with the root user's static credentials. `expiry_seconds` and `read_write` are validated; a malformed one is a 422. The agent asks for credentials here for each machine. As on Vultr, other methods get a 405. |
| Metadata service, inside each instance: `GET http://169.254.169.254/v1.json` and `/v1/` | `v1.json` has `hostname`, `instanceid`, `instance-v2-id`, `interfaces` (the public one and, in a VPC, a private one with `network-v2-id`), `public-keys`, `region` (`regioncode`, `countrycode`) and `bgp` with empty values, plus `user-data` and `vendor-data`, which cloud-init's Vultr datasource reads. The tree has `hostname`, `instanceid`, `instance-v2-id`, `public-keys`, `region/` and `interfaces/<n>/` with `network-type`, `network-v2-id` and `ipv4/address`, `gateway` and `netmask`. |

Every request behaves as follows:

- **Routing and authentication.** An unknown path is a 404 `{"error": "Not found.", "status": 404}` and a known path with another method a 405 that names the allowed methods, both before the key is checked, as on Vultr. Then `Authorization: Bearer <key>` must name an accepted key, or the answer is a 401 `{"error": "Invalid API token.", "status": 401}`.
- **Errors.** `{"error": message, "status": code}` with 400 for validation and conflicts, 404 for missing objects, 422 for malformed registry credential options and 500 for a failed machine operation. Missing objects use the messages the Terraform provider looks for, such as `Invalid VPC ID.`, `Invalid ssh key ID.`, `Invalid reserved-ip ID.`, `Firewall rule ID not found.` and `Invalid container registry ID.`.
- **Rate limit.** 30 requests a second from each client address; more get a 429 with `Retry-After: 1`.
- **Pagination.** `per_page` (default 100, at most 500; a value that is not a positive number means the default) and `cursor`. Lists have `meta.total` and `meta.links.next` and `prev`, opaque cursors that are empty at either end. An unknown cursor starts from the first page, as on Vultr.
- **Firewall groups.** Vultr's firewall filters only inbound traffic on the public interface and never filters outbound traffic or a VPC. So the machine of an instance in a group drops every new connection that no IPv4 rule allows, on its host ports and the ports Docker publishes alike. In a VPC, it lets in all traffic from the private Docker network, which plays the VPC. Outside a VPC, the instance has no such network, so traffic from it is dropped too. An instance without a group accepts all traffic. A new instance boots with its group's rules; changes reach running machines within seconds, and failed pushes are retried every 5 seconds.

### Differences from Vultr

- All API keys act on one account, and the rate limit counts per client address whatever the key.
- Plans are labels: machines are not limited to the plan's CPUs, memory or disk. All regions share one Docker host.
- VPCs do not isolate instances. Every private address comes from the `-vultr-network` network, whatever the VPC's subnet says, and an instance outside any VPC has an address there too, though it reports no `internal_ip`. The firewall group of an instance in a VPC lets in every address on that Docker network, including containers that are not instances, such as a check's client.
- Only Ubuntu 24.04 (`os_id` 2284) and snapshots boot. ISO, application, image and iPXE installs, startup scripts, IPv6, `enable_vpc`, VPC 2.0, NAT gateways (`vpc_only`), automatic backups, DDoS protection, the limited user, block devices and marketplace variables get a 400. So do firewall rules for `gre`, `esp` and `ah` and rules with a `source`. IPv6 firewall rules are kept but have no effect: machines have IPv4 only.
- Endpoints the simulation does not serve, such as instance start, halt and reboot, DNS, load balancers and registry repositories, answer 404 `Not found.`, as an unknown path does.
- Create of a reserved IP ignores `instance_id`, which the Terraform provider sends although the API reference lists no such field; the provider then attaches the address. The main address of an instance created with `reserved_ipv4` can't be detached or deleted until the instance is deleted. Its machine has the reserved IP as its public address.
- While an instance is destroyed, the simulation answers 400 with `Server is pending destruction.`, the message the Terraform provider's tests expect; the API reference does not document the status code. A machine that cannot be created leaves its instance `suspended` and `stopped`.
- Deleting a firewall group or a reserved IP that instances use succeeds and takes it off them; the API reference does not say what Vultr does.
- The metadata service is fixed when the instance is created. It has no MAC addresses, BGP values or `nvidia-driver`, its `vendor-data` is empty, and reserved IPs attached later are not listed.
- A snapshot does not include `/var/lib/docker`, which is a volume. Simulated machines have no `shutdown` command, so Packer's graceful shutdown step fails quietly and the plugin snapshots the running machine.
- Every registry's root user has the `-vultr-registry-*` credentials, and `docker-credentials` hands them out whatever `expiry_seconds` and `read_write` say. Registries can be in any region. Repositories, robot accounts, replications and retention rules are not simulated.
- The simulation does not publish ports on the Docker host; run clients in containers on the networks.

### Checks

```sh
go test ./internal/vultr                                   # no Docker
mise exec -- sim/scripts/check-vultr.sh agent              # needs Docker, Go and openssl
mise exec -- sim/scripts/check-vultr.sh terraform          # needs Docker, Go and openssl
mise exec -- sim/scripts/check-vultr.sh image              # needs Docker, Go and openssl
```

Run the scripts from the repository root. Each builds `stagely-sim` for Linux, makes a CA and a certificate for `api.vultr.com`, and runs the simulation in a `docker:29.8.1-cli` container with the Docker socket, on its own public network under the alias `api.vultr.com`. Its port is published on `127.0.0.1:9390` (`STAGELY_SIM_CHECK_PORT` changes it). The script removes the containers, networks, machines and snapshot images of its run when it ends.

- `agent` cross-compiles the `simdocker` test of the agent's adapter (`internal/agent/provider/vultr/sim_test.go`) and runs it in an `ubuntu:24.04` container on both networks that trusts the CA. The unchanged adapter calls `https://api.vultr.com`. As setup's module does, the test creates a VPC whose range is the private Docker network's and a firewall group without rules, and lets only its own private address, the agent's here, connect to instances in the VPC. The adapter gets push and pull registry credentials for a registry the test creates, registers its SSH key, and creates a stagelet and a neighbor instance in the group and the VPC, with user data that holds the host keys and the rules for the VPC, connects over SSH on their private addresses, and waits for `cloud-init status --wait` and `docker compose version`. On the stagelet it builds and starts a Compose project that pulls `busybox` from Docker Hub. The test fetches a page from it on its private address, checks that the neighbor can connect out to `api.vultr.com` and to the test over the VPC but not to the stagelet's published port or SSH port, which only the stagelet's own rules refuse, and that a container on the stagelet can't connect to the test over the VPC or reach the metadata service. The adapter deletes both instances, waiting until `GET` answers 404. The script then checks that no machine is left.
- `terraform` runs OpenTofu 1.12.6 in the `ghcr.io/opentofu/opentofu:1.12.6` container with `vultr/vultr` 2.32.0, the version that `terraform/vultr` pins. It applies `internal/vultr/testdata/terraform`: an SSH key, a firewall group with IPv4 and IPv6 rules, two instances in the group, a reserved IP attached to one, a container registry and the `vultr_os` data source. Then it applies `terraform/vultr`, which adds a VPC, with the variables `stagely setup` passes. For each it checks that two machines exist, that `tofu plan -detailed-exitcode` finds no changes, and that no machine is left after `tofu destroy`. It downloads the provider.
- `image` builds the golden image in `images/vultr` with Packer 1.16.1 and its `vultr` plugin 2.7.0, in a container on both networks that trusts the CA: a build instance, the provisioners, a snapshot, and the build instance's deletion. It checks that no machine is left, then runs the `agent` check with `STAGELY_SIM_VULTR_GOLDEN_IMAGE` set to the snapshot, so the adapter's instance boots from it.

### Sources

- Vultr API reference and its OpenAPI document (version 2.0), as [vultr.com/api](https://www.vultr.com/api/) served it on 2026-09-12: paths, request and response schemas, the error shape, pagination with `per_page` and `cursor`, and the rate limit. Answers of the live API on 2026-10-11 for `GET /v2/os`, `/v2/regions` and `/v2/plans`, which need no key, and for an unknown path, a wrong method and an invalid key.
- [govultr](https://github.com/vultr/govultr) v3.31.2: request and response types, `Meta` and `Links`, and `DoWithContext`, which hands the error body to callers as the error text
- [terraform-provider-vultr](https://github.com/vultr/terraform-provider-vultr) v2.32.0: the calls and fields of `vultr_instance`, `vultr_vpc`, `vultr_ssh_key`, `vultr_firewall_group`, `vultr_firewall_rule`, `vultr_reserved_ip`, `vultr_container_registry` and `vultr_os`, the error messages it treats as a missing object or retries on, and its configuration, which has no API URL
- [packer-plugin-vultr](https://github.com/vultr/packer-plugin-vultr) v2.7.0: the builder's steps, from the temporary SSH key to the snapshot, and its configuration, which has no API URL
- [Vultr metadata API](https://www.vultr.com/metadata/) as served on 2026-05-19, and cloud-init 26.1's `DataSourceVultr.py`, `sources/helpers/vultr.py` and its unit tests for the keys of `v1.json` that cloud-init reads
- Vultr docs: [enable a firewall group](https://docs.vultr.com/products/compute/instances/cloud-gpu/networking/enable-firewall) ("filter incoming traffic on an instance's public network interface"), the [firewall FAQ](https://docs.vultr.com/products/network/firewall-groups/faq) (no outbound filtering), and the [container registry plans](https://docs.vultr.com/support/products/container-registry/what-are-the-available-plans-for-vultr-container-registry)

## Azure

The Azure simulation serves Azure Resource Manager, its metadata document for custom clouds, and the client credentials grant of the Microsoft Entra ID token endpoint, all on one address. Each virtual machine is a [machine](#machines), and its network security groups are enforced inside the machine. The container registry is a CNCF Distribution registry (`registry:2`) that you run under `<registry name>.azurecr.io` on the simulated network; the simulation keeps the registry's management record and hands out its credentials.

```sh
go run ./cmd/stagely-sim -azure -azure-tls-cert sim.pem -azure-tls-key sim-key.pem
```

The Terraform provider reads the metadata document only over HTTPS, so give the simulation a certificate that clients trust. Then point clients at `https://<host>:9305` with the tenant `0000a2e0-0000-4000-8000-000000000001`, the subscription `0000a2e0-0000-4000-8000-000000000002`, the client id `0000a2e0-0000-4000-8000-000000000003` and the secret `stagely-sim-secret`: the Terraform provider with `metadata_host = "<host>:9305"`, the Packer plugin with `metadata_host = "https://<host>:9305"`, the agent with `-azure-arm-url`, and `stagely setup azure` with `-azure-arm-url`. Each client reads the login endpoint and the token audience from the metadata document. State is kept in `<data-dir>/azure/state.json`.

At start, the simulation matches stored virtual machines with the machines Docker has: a virtual machine takes its power state from its machine, a virtual machine whose machine is gone is removed with a warning, a create or delete that a restart interrupted ends `Failed`, and a machine of this simulation that no virtual machine owns is deleted. Machines carry the labels `stagely-sim.cloud=azure`, `stagely-sim.account=<account uuid>` and `stagely-sim.resource=<vmId>`, and are named `stagely-sim-az-<account>-<vmId>`.

### Flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `-azure` | off | Run the Azure simulation |
| `-azure-addr` | `127.0.0.1:9305` | Listen address |
| `-azure-tls-cert`, `-azure-tls-key` | none | Certificate and key to serve HTTPS with; plain HTTP without them |
| `-azure-tenant` | `0000a2e0-0000-4000-8000-000000000001` | Tenant that the token endpoint serves |
| `-azure-subscription` | `0000a2e0-0000-4000-8000-000000000002` | The one subscription |
| `-azure-client-id`, `-azure-client-secret` | `0000a2e0-0000-4000-8000-000000000003`, `stagely-sim-secret` | The one service principal |
| `-azure-locations` | `eastus,eastus2,westus2,westus3,centralus,northeurope,westeurope,uksouth,germanywestcentral,swedencentral,francecentral,southeastasia,australiaeast,canadacentral,japaneast` | Locations that resources may use |
| `-azure-network` | `stagely-sim-azure` | Docker network for private addresses. Public addresses use `<network>-public`. |
| `-azure-registry-domain` | `azurecr.io` | Domain of registry login servers |
| `-azure-registry-username`, `-azure-registry-password` | `stagely`, `stagely` | The user name of generated token credentials, and the password of every credential the simulation hands out |

### Coverage

| Endpoint | Notes |
| --- | --- |
| `POST /{tenant}/oauth2/v2.0/token` | `client_credentials` with a `scope` ending in `/.default`. The access token is a JWT with `aud`, `iss`, `tid`, `appid`, `oid` (stable for the client), `idtyp`, `iat`, `nbf` and `exp` one hour later. Errors use the identity platform's shape and AADSTS codes: unknown tenant 90002, unknown client 700016, wrong secret 7000215 (401), bad scope 1002012. |
| `GET /metadata/endpoints?api-version=` | `resourceManager`, `authentication.loginEndpoint` and `audiences` are the address the client used; `microsoftGraphResourceId`, and `suffixes` with `acrLoginServer`. API versions from `2022-09-01` get one object; older ones get an array of it. |
| `GET /subscriptions/{sub}` | |
| Resource groups: `GET` list, `PUT`, `GET`, `HEAD`, `PATCH` (tags), `DELETE`, `GET .../resources` | Deleting a group deletes everything in it, the machines first, as a long-running operation. |
| `Microsoft.Network/virtualNetworks` and `.../subnets`: `PUT`, `GET`, `DELETE`, lists | IPv4 address spaces; subnets must fit them without overlap. A network PUT replaces its subnets. `addressPrefix` or `addressPrefixes` is echoed as sent. A subnet takes a network security group and `defaultOutboundAccess`. |
| `Microsoft.Network/networkSecurityGroups`: `PUT`, `GET`, `DELETE`, list | Rules inline, with Azure's six default rules in `defaultSecurityRules`. Priorities 100 to 4096, unique for each direction; protocols `Tcp`, `Udp`, `Icmp`, `*`; port ranges; IPv4 prefixes and the tags `*`, `Any`, `Internet`, `VirtualNetwork`, `AzureLoadBalancer`. |
| `Microsoft.Network/publicIPAddresses`: `PUT`, `GET`, `DELETE`, list | Standard SKU with static allocation only; Basic is refused, as Azure refuses it since September 2025. The address comes from the middle of the public network upward. `dnsSettings.domainNameLabel` gives an `fqdn`. |
| `Microsoft.Network/networkInterfaces`: `PUT`, `GET`, `DELETE`, list | One IP configuration with dynamic allocation, a subnet and an optional public IP; an optional network security group. The private address comes from the top of the private network down when the interface is created, and the machine boots on it. |
| `Microsoft.Compute/virtualMachines`: `PUT`, `GET` (`$expand=instanceView`, `userData`), `PATCH` (tags), `DELETE`, `GET .../instanceView`, lists in a group and in the subscription | Linux with SSH keys at `/home/<admin>/.ssh/authorized_keys`; reserved admin names are refused. `storageProfile.imageReference` is `Canonical:ubuntu-24_04-lts:server`, `server-gen1` or `minimal` at `latest` or `24.04.202609240`, or a managed image of the subscription. The OS disk comes from the image and is created as a managed disk; `deleteOption` of the OS disk and the network interface is honored. `customData` is never returned. Sizes: see `catalog.go`. |
| `POST .../virtualMachines/{name}/powerOff`, `deallocate`, `start`, `restart`, `generalize` | Power actions are long-running operations. `generalize` needs a stopped machine, and a generalized machine never starts again. |
| `Microsoft.Compute/disks`: `GET`, `DELETE`, list | OS disks only. An attached disk cannot be deleted. |
| `Microsoft.Compute/images`: `PUT`, `GET`, `DELETE`, list | From `sourceVirtualMachine`, which must be generalized. The capture commits the machine into the Docker image `stagely-sim-az-image:<account>-<guid>`; deleting the image removes it. |
| `Microsoft.ContainerRegistry/registries`: `PUT`, `GET`, `PATCH`, `DELETE`, `POST .../listCredentials`, `GET .../replications`, `POST /subscriptions/{sub}/providers/Microsoft.ContainerRegistry/checkNameAvailability` | The login server is `<name>.<-azure-registry-domain>`. With the admin user on, its credentials are the registry name and `-azure-registry-password`. |
| Registry tokens: `PUT`, `GET`, `DELETE` `.../tokens/{name}`, `GET .../tokens`, `GET .../scopeMaps/{name}`, `POST .../generateCredentials` | Tokens over the built-in scope maps `_repositories_pull`, `_repositories_push`, `_repositories_admin` and the `_metadata` variants. Generated passwords record their expiry. |
| `Microsoft.Resources/deployments`: `PUT`, `GET`, `DELETE`, `POST .../validate`, `GET .../operations` (also under `resourceGroups/{group}/deployments`) | Incremental template deployments of public IPs, networks, security groups, network interfaces and virtual machines, in `dependsOn` order, through the same handlers as direct requests. Template functions: `parameters`, `variables`, `concat`, `format`, `resourceId`, `resourceGroup`, `subscription`, `not`, `empty`, `equals`, `if`, `toLower`, `toUpper`, and property access. Resources with a false `condition` are skipped. |
| Operation links: `GET /subscriptions/{sub}/providers/{namespace}/locations/{location}/operations/{id}` and `.../operationResults/{id}` | The `Azure-AsyncOperation` link answers `{"status": "InProgress" \| "Succeeded" \| "Failed"}`; the `Location` link answers 202 while running, then 204, or the error. |
| Instance metadata, inside each machine: `GET http://169.254.169.254/metadata/instance` | The instance document with `compute` (name, location, vmId, vmSize, resource group, subscription, image, `osProfile`, `publicKeys`, tags, `userData`) and `network` (private and public addresses, the subnet as declared, MAC address), each part at its own path and every leaf as text. |

Every Resource Manager request behaves as follows:

- **Authentication.** `Authorization: Bearer <token>` from the token endpoint. A missing, forged or expired token, or one issued for Microsoft Graph, gets a 401 with `AuthenticationFailed`, `InvalidAuthenticationToken`, `ExpiredAuthenticationToken` or `InvalidAuthenticationTokenAudience`, and a `WWW-Authenticate` header that names the tenant.
- **API versions.** Every request needs `api-version` (`MissingApiVersionParameter`, `InvalidApiVersionParameter`). Any dated version is served the same way.
- **Errors.** `{"error": {"code", "message"}}` with Resource Manager's codes, such as `ResourceGroupNotFound`, `ResourceNotFound`, `InvalidParameter`, `InUseSubnetCannotBeDeleted`, `NicInUse`, `PublicIPAddressCannotBeDeleted`, `InUseNetworkSecurityGroupCannotBeDeleted` and `OperationNotAllowed`. A request for something the simulation does not support gets a 400 `UnsupportedBySimulation` that names it, or `InvalidResourceType` for a path it does not serve, which it also logs. Every response has `x-ms-request-id`.
- **Paths.** Literal path segments match without case, as in Resource Manager.
- **Long-running operations.** Writes and deletes answer with `Azure-AsyncOperation` (and for deletes and actions `Location`) and `Retry-After: 1`. Network, registry and group writes finish at once; virtual machine creates, deletes and power actions, image captures, group deletes and deployments run until their Docker work ends. Deleting a missing network or compute resource answers 204.
- **Paging.** Lists answer `value` and, past 100 items, a `nextLink` with `$skiptoken`.
- **Provisioning.** A virtual machine is `Creating` until its machine runs, the simulation has created the admin user from `osProfile` with passwordless sudo and the keys, and the machine has its public IP addresses; then `Succeeded`. It is `Deleting` until its machine is gone, so `GET` answers 404 only after that.
- **Network security.** Each machine enforces what its subnet's and its network interface's security groups both allow, rules in priority order with the default rules last, for the machine's private address. `VirtualNetwork` stands for the private network. Without a public IP, or with a Standard public IP but no security group, only the private network reaches the machine, as on Azure. A subnet with `defaultOutboundAccess: false` keeps a machine without a public IP from connecting outside the private network. Changes reach running machines at once; failed pushes are retried every 5 seconds.
- **Public IPs.** A machine answers at the public IPs of its network interface as extra addresses on its public interface.

### Differences from Azure

- One subscription and one service principal. Tokens are signed with HS256 by the simulation and carry no roles; any valid token may do anything. The token audience is checked only against Microsoft Graph.
- No throttling, quotas or regional capacity. Sizes are labels, all locations share one Docker host, and zones are echoed without effect.
- Address prefixes do not map to addresses: every private address comes from the `-azure-network` network, whatever the virtual network and subnet say, and a security rule that names a declared range or a host outside the private network matches no machine. Use `VirtualNetwork`, `*` or the addresses that network interfaces report.
- A machine also answers on its own address on the public network, under the same rules as its public IPs.
- No IPv6, load balancers, application security groups, route tables, NAT gateways, service endpoints, delegations, peerings, DDoS protection, availability sets, scale sets, data disks, ephemeral or unmanaged disks, Windows, password logins, Spot priority, managed identities, marketplace plans, extensions, gallery images, snapshots, or custom scope maps. Requests that use them get `UnsupportedBySimulation`.
- Custom data reaches the machine through NoCloud seed data, not the OVF environment of a provisioning disk. The machine does not report ready to the wireserver at `168.63.129.16`, which it does not serve; the simulation marks the virtual machine `Succeeded` when its admin user exists.
- The machine's cloud-init stand-in gives the `osProfile` keys to root as well, as with `disable_root: false`, which the agent and the bootstrap module send. On Azure, root gets the keys only with that setting.
- The instance metadata service ignores the `Metadata: true` header, the `api-version` and `format` parameters, and serves the instance document as `text/plain`. It has no `identity/oauth2/token` and no scheduled events, and is fixed when the machine is created.
- An image capture does not include `/var/lib/docker`, which is a volume.
- The registry is a stock registry with static users, so token passwords are always `-azure-registry-username` and `-azure-registry-password`: their expiry and scope are recorded, not enforced. Token limits are not enforced.
- Deployments run their resources one at a time, support no `copy`, nested resources or outputs, and `validate` checks only the template.
- A subnet without `defaultOutboundAccess` gets default outbound access, as with API versions released before April 2026; newer API versions make subnets of new networks private. Changing the setting applies to running machines at once; Azure applies it after a machine is deallocated and started.
- The machine has no wireserver at `168.63.129.16`: a connection there leaves through the public network and gets no answer.

### Checks

```sh
go test ./internal/azure                                 # no Docker
mise exec -- sim/scripts/check-azure.sh agent            # needs Docker, Go and openssl
mise exec -- sim/scripts/check-azure.sh terraform        # needs Docker, Go and openssl
mise exec -- sim/scripts/check-azure.sh image            # needs Docker, Go and openssl
```

Run the scripts from the repository root. Each makes a CA and a certificate for `stagely-sim-host`, starts its own simulation over HTTPS with a temporary data directory, its own Docker networks and port 9395 (`STAGELY_SIM_CHECK_PORT` changes it), and removes all of it when it ends. Clients run in containers that trust the CA through `SSL_CERT_FILE` and reach the simulation as `stagely-sim-host` through `host-gateway`.

- `agent` cross-compiles the `simdocker` test of the agent's adapter (`internal/agent/provider/azure/sim_test.go`) and runs it in a container on both simulation networks. The test makes a resource group, a stagelet subnet with a network security group that lets in only the test's addresses and denies the rest, a private subnet beside it, and a registry. The unchanged adapter creates a stagelet and a neighbor, connects over SSH with the host keys it put in the custom data, waits for `cloud-init status --wait` and `docker compose version`, builds and starts a Compose project that pulls `busybox` from Docker Hub, fetches a page from it on the stagelet's private address, checks that the neighbor can connect out but not to the stagelet's port, that a third machine in a subnet with `defaultOutboundAccess: false` cannot connect out, that a container cannot reach the metadata service, and that a container's connection to the wireserver's HTTP port is refused while its DNS port is not, gets pull and push registry credentials, and deletes the machines, waiting until `GET` answers 404. The script then checks that no machine is left.
- `terraform` applies `terraform/azure` with OpenTofu 1.12.6 (the `ghcr.io/opentofu/opentofu:1.12.6` image) and `hashicorp/azurerm` 5.7.0, the version the module pins. It checks that two machines exist, that `tofu plan -detailed-exitcode` finds no changes, and that no machine is left after `tofu destroy`. It downloads the provider.
- `image` builds the golden image in `images/azure` with Packer 1.16.1 and its `azure` plugin 2.6.4, in a container on both simulation networks: a temporary resource group, a template deployment of the build machine with its network, the provisioners over SSH on its public IP, deallocation, generalization, the image capture, and the deletion of each resource and the group. It checks that no machine is left, then runs the `agent` check with `STAGELY_SIM_AZURE_GOLDEN_IMAGE` set to the image, so the adapter's machines boot from it.

### Sources

- Azure Resource Manager: [REST API reference](https://learn.microsoft.com/rest/api/azure/), [async operations](https://learn.microsoft.com/azure/azure-resource-manager/management/async-operations), [error codes](https://learn.microsoft.com/azure/azure-resource-manager/troubleshooting/common-deployment-errors), [throttling](https://learn.microsoft.com/azure/azure-resource-manager/management/request-limits-and-throttling), and the OpenAPI specs in [Azure/azure-rest-api-specs](https://github.com/Azure/azure-rest-api-specs) for `network`, `compute`, `containerregistry` and `resources`
- Networking: [network security groups and their default rules](https://learn.microsoft.com/azure/virtual-network/network-security-groups-overview), [service tags](https://learn.microsoft.com/azure/virtual-network/service-tags-overview), [Standard public IPs](https://learn.microsoft.com/azure/virtual-network/ip-services/public-ip-addresses), [Basic SKU retirement](https://azure.microsoft.com/updates/upgrade-to-standard-sku-public-ip-addresses-in-azure-by-30-september-2025-basic-sku-will-be-retired/), [default outbound access](https://learn.microsoft.com/azure/virtual-network/ip-services/default-outbound-access)
- Compute: [Linux VM user names and SSH keys](https://learn.microsoft.com/azure/virtual-machines/linux/faq), [custom data and cloud-init](https://learn.microsoft.com/azure/virtual-machines/custom-data), [capturing a managed image](https://learn.microsoft.com/azure/virtual-machines/capture-image-resource), [Instance Metadata Service](https://learn.microsoft.com/azure/virtual-machines/instance-metadata-service)
- Container Registry: [repository-scoped tokens and scope maps](https://learn.microsoft.com/azure/container-registry/container-registry-repository-scoped-permissions), [admin account](https://learn.microsoft.com/azure/container-registry/container-registry-authentication#admin-account)
- Identity: [client credentials grant](https://learn.microsoft.com/entra/identity-platform/v2-oauth2-client-creds-grant-flow), [access token claims](https://learn.microsoft.com/entra/identity-platform/access-token-claims-reference), [AADSTS error codes](https://learn.microsoft.com/entra/identity-platform/reference-error-codes)
- [hashicorp/go-azure-sdk](https://github.com/hashicorp/go-azure-sdk) v0.20260917.1142820: `sdk/environments` and `sdk/internal/metadata` for the metadata document, `sdk/auth` for the token request, `sdk/claims`, and `sdk/client/resourcemanager` for the long-running operation pollers
- [terraform-provider-azurerm](https://github.com/hashicorp/terraform-provider-azurerm) v5.7.0: `metadata_host` and `resource_provider_registrations` in `internal/provider`, the account built from token claims in `internal/clients`, and the calls and fields of `azurerm_resource_group`, `azurerm_virtual_network`, `azurerm_subnet`, `azurerm_network_security_group`, the association resources, `azurerm_public_ip`, `azurerm_network_interface`, `azurerm_linux_virtual_machine` and `azurerm_container_registry`
- [packer-plugin-azure](https://github.com/hashicorp/packer-plugin-azure) v2.6.4: `builder/azure/common/client` for `metadata_host`, `builder/azure/common/template` and `builder/azure/arm` for the deployment template, its steps, the capture and the cleanup
- cloud-init's [Azure datasource](https://docs.cloud-init.io/en/latest/reference/datasources/azure.html) for what Azure's provisioning does inside the VM

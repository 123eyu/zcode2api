# SDK verification and log errors

[Back to README](../README.EN.md) · [Proxy and verification reference (Chinese)](network.md) · [Log settings (Chinese)](configuration.md#日志保存与筛选)

## SDK verification

The server runs the official Alibaba Cloud SDK using Node.js 18+ and jsdom; Chrome is not required. Select an inference verification policy under **设置 → 上游安全验证** (Settings → Upstream security verification), then save:

| Policy | Behavior |
| --- | --- |
| `off` (default) | Do not run the verification SDK for inference |
| `auto` (recommended) | Follow the upstream settings for enabled verification scenes and verification exemptions; obtain a ticket when verification is explicitly required |
| `always` | Obtain a ticket before each inference request when the upstream verification scene is enabled |

When inference verification is enabled, an expired ticket can trigger one fresh verification on the same egress route if no output has been sent. Once response text, reasoning or tool calls have been sent, the full request is not replayed.

For single-use tickets, prefetching, cooldowns and the official interactive verification flow, see the [security verification reference (Chinese)](network.md#安全验证). Verification outcomes are determined by the upstream service.

### Local installation

For automatic verification outside Docker, install Node.js 18+ (including npm) and run from the project or extracted release root:

```sh
npm --prefix captcha_node ci --omit=dev --ignore-scripts
```

### Docker

Build the image with the Node.js and jsdom runtime.

```sh
docker compose build --build-arg CAPTCHA_RUNTIME=1
docker compose up -d
```

## Log errors

In request logs, `error_message` is a categorized summary in Chinese, and `error_code` is the specific error code.

| Error code / message | Meaning and action |
| --- | --- |
| `upstream_waf_blocked` | Explicit upstream security gateway block; put the current egress route on cooldown and switch proxies when a usable backup node is available |
| `proxy_connection_failed` / `proxy_auth_failed` | Proxy connection or authentication failure; check the proxy address, service status and credentials. When using a proxy pool, the node is temporarily quarantined |
| `proxy_pool_exhausted` / `proxy_upstream_blocked` | No usable proxy nodes remain, or the egress route is still on cooldown after an upstream block; check nodes or wait for the cooldown to expire |
| `3012` / `upstream_risk_blocked` | Treated as an account-level risk-control rejection; the request may use another available account. This does not remove the proxy or automatically disable the account, and is not evidence of a permanent ban |
| `3008` / `3009` / `3010` | The upstream account is temporarily at its concurrency limit; prefer another available account, or queue until capacity is available |
| `account_busy` | The gateway timed out waiting for an available account slot; check account count, concurrency usage and queue wait time |
| `rate_limit_error` / request rate limited | Upstream rate limiting; respect the wait time and retry window. This does not indicate a broken proxy |
| `authentication_error` / `insufficient_quota` | Upstream account credentials are invalid, or the account has insufficient quota, respectively; reauthorize or add usable quota. The request may use another available account |
| `coding_plan_security_verification_required`, or `3007` with an explicit verification message | The upstream service requires security verification; use the corresponding SDK or manual verification flow. Interpret `3007` together with its message; it does not always indicate a CAPTCHA issue |
| `captcha_risk_blocked` / `F001` | SDK risk-control rejection; this does not establish that the account or proxy is invalid, and does not trigger a proxy node switch |
| `F010` / `F011` | SDK request rate limited for the egress route or device, respectively; apply cooldown to the corresponding scope |
| `captcha_expired`, `F019`, `F008`, `F018` | Verification ticket expired or submitted more than once; obtain a fresh ticket. A submitted result cannot be reused |
| `captcha_jsdom_missing` / `captcha_runtime_error` / `captcha_timeout` | Missing SDK dependency, runtime error or verification timeout; check Node.js, jsdom, network connectivity and verification settings |
| `first_token_timeout` / `stream_idle_timeout` / `inference_timeout` | Timeout waiting for the first meaningful output, timeout while a stream is idle, or timeout for the entire inference request, respectively |
| `proxy_subscription_auth_failed` | Proxy subscription authentication failed; the token may be incorrect or expired. Copy a valid subscription URL again |

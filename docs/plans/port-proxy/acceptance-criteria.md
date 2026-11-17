# Port proxy: acceptance criteria for forwarding

Status: `PP-4` to `PP-13` met (#393), the hostname form of issue #388. `PP-3` met (#382), the opt-in
wait for a waking environment. `PP-1` met by #277, which fixed the second bug a property test found
against it (#245 fixed the first). `PP-2` met by #316, forwarding the raw query and bare `?`
unchanged. Each criterion names the tests that check it on every pull request (the Go job's
`go test -race ./...`).

## Scope

ate-env-api's port proxy
([internal/portproxy/portproxy.go](../../../internal/portproxy/portproxy.go)) forwards an HTTP or
WebSocket request to a port a daemon listens on inside an environment. A caller addresses the
environment and port in one of three forms:

- **Header form:** `X-Env-Id`, `X-Env-Port` (and `X-Env-Atespace`) headers. The proxy forwards the
  request's own path and query.
- **Path form:** `/v1alpha/envs/<id>/ports/<port>/<rest>` (or
  `/v1alpha/atespaces/<atespace>/envs/<id>/ports/<port>/<rest>`), for browsers and WebSocket clients
  that cannot set headers. The proxy forwards `/<rest>` and the query.
- **Hostname form:** the `Host` is `<port>-<id>.<domain>` (or `<port>-<id>.<atespace>.<domain>`),
  for clients that need a plain URL for a port. It is off unless `--port-proxy-host-suffix <domain>`
  is set. The proxy forwards the request's own path and query. `PP-4` to `PP-13` are its criteria;
  the section "Hostname form" has the decisions behind them.

`Dispatch` matches the path form itself rather than on a `ServeMux` "so the daemon gets the path
exactly as sent": a mux would redirect a missing trailing slash, `//` and `..`, and WebSocket
clients do not follow redirects.

`PP-1` has one exception: a forwarded path that starts with `//` would read as an authority if
written as is, so in it the proxy percent-encodes the bytes `URL.RawPath` cannot hold raw
(`{ } | ^ " < > \`, `` ` ``, `#`, space, control bytes and non-ASCII) and keeps every other byte,
escapes included. The daemon gets the same segments, each decoding to the same bytes, and gets the
query byte for byte (`PP-2`).

## Criteria

| ID    | Criterion                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                   | Status      | Tests                                                                                                                                                                                                                                                                                                                                                                           |
| ----- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ----------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| PP-1  | The daemon gets the forwarded path exactly as the caller sent it, byte for byte, in both forms: the path form's `/<rest>` after the routing prefix, and the header form's path. No character is escaped, unescaped or cleaned on the way.                                                                                                                                                                                                                                                                                                                                                                                                                   | met by #277 | `TestPathFormDeliversPathAsSentProperty`, `TestPathFormKeepsSubDelimsAsSent`, `TestPathFormKeepsPathAsSent`, `TestRawBytesKeptAsSent`, `TestRawBytesDoNotMoveRoutingPrefix`                                                                                                                                                                                                     |
| PP-2  | The daemon gets the query of the request-target (including a bare trailing `?`, `;` parameters, bad `%` escapes, `{ } \| ^`, `#`, and raw UTF-8) exactly as the caller sent it, byte for byte, in both forms, including on `//` paths.                                                                                                                                                                                                                                                                                                                                                                                                                      | met by #316 | `TestPathFormDeliversPathAsSentProperty`, `TestRawBytesKeptAsSent`, `TestRawQueryKeptAsSent`                                                                                                                                                                                                                                                                                    |
| PP-3  | A request with `X-Env-Wake-Wait: <seconds>` (whole decimal seconds) is held while its environment wakes, for at most the smaller of that value and `--port-proxy-wake-wait-cap`, and is forwarded once, body whole, after the guest answers; nothing is forwarded before. A wait that ends while the environment wakes gets the 503 with `Retry-After: 1` of a request without the header; a crashed environment gets 410 and a deleting one 404 at once. An invalid or repeated header gets 400. The daemon never gets the header. A caller that leaves ends the wait. Without the header the answer is unchanged, and the wait is not called. Both forms. | met (#382)  | `TestNoWakeWaitHeaderAnswersAsBefore`, `TestWakeWaitHoldsUntilTheEnvironmentServes`, `TestWakeWaitExpiresWithTheSame503`, `TestWakeWaitCap`, `TestInvalidWakeWaitIs400`, `TestWakeWaitEndsAtOnceForAnEnvironmentThatWillNotWake`, `TestCallerLeavingEndsTheHold`, `TestCallerLeavingEndsTheHoldOverHTTP2`, `TestWakeWaitNeedsTheCredential`, `TestWake` (`internal/apiservice`) |
| PP-4  | With `--port-proxy-host-suffix <domain>`, a request whose `Host` is `<port>-<id>.<domain>` reaches that port of that environment in the default atespace, and `<port>-<id>.<atespace>.<domain>` reaches it in the named atespace. The daemon gets the whole path and the query as sent (`PP-1`, `PP-2`). The proxy reads the `Host` without its port, without one trailing dot and without regard to ASCII case. The port is the part of the first label before its first `-`, thus an id can hold `-`.                                                                                                                                                     | met (#393)  | `TestHostFormForwardsToThePort`, `TestHostFormKeepsPathAndQueryAsSent`                                                                                                                                                                                                                                                                                                          |
| PP-5  | Without `--port-proxy-host-suffix` each answer is the one `main` gave before the flag existed: a `Host` of the hostname shape names no target, `Dispatch` routes on the headers and the path only, and behind `internal/hostcheck` such a `Host` gets the 403 of an unknown host.                                                                                                                                                                                                                                                                                                                                                                           | met (#393)  | `TestHostFormOffByDefault`, `TestHostFormListenerStack` (`internal/hostcheck`)                                                                                                                                                                                                                                                                                                  |
| PP-6  | A `Host` that is the suffix or a name below it and is not a valid target gets 400 with the expected shape in the body, and reaches no daemon, no other form and no API route. Invalid are: the bare suffix; more than two labels in front of the suffix; a first label with no `-`; a port that is not a canonical TCP port, or port 80; an id or atespace that is not a DNS label; a first label longer than 63 bytes; a byte outside ASCII, also one that Unicode case folding maps to an ASCII letter.                                                                                                                                                   | met (#393)  | `TestHostFormRefusesBadHosts`, `TestHostFormRefusesNonASCIIHost`, `TestParseHost`                                                                                                                                                                                                                                                                                               |
| PP-7  | A request takes exactly one form. Under the suffix the `Host` alone names the target: a request that also carries `X-Env-Id`, `X-Env-Atespace` or `X-Env-Port` gets 400, and a path of the path form's shape goes to the daemon whole, as any other path. A request under the suffix never reaches the API routes, gRPC included.                                                                                                                                                                                                                                                                                                                           | met (#393)  | `TestHostFormTakesExactlyOneForm`                                                                                                                                                                                                                                                                                                                                               |
| PP-8  | `internal/hostcheck` lets a name under the suffix pass only on the port proxy's route, only with the flag set and only when the name is a valid target. Each other `Host` gets the answer it got before: an unknown name gets 403 on `/healthz`, on MCP and on each form of the port proxy, over HTTP/1.x and h2c.                                                                                                                                                                                                                                                                                                                                          | met (#393)  | `TestWrapFor`, `TestHostFormListenerStack` (`internal/hostcheck`)                                                                                                                                                                                                                                                                                                               |
| PP-9  | The hostname form checks the caller as the path form does, before anything is forwarded: no credential gets 401; the token passes in `Authorization` or in `X-Env-Authorization`; a port capability passes only on a WebSocket handshake and only for the atespace, id and port that the `Host` names. An atespace outside `--port-proxy-atespaces` gets 403, also the default atespace of the short name. The daemon gets no API credential and no `X-Env-*` header.                                                                                                                                                                                       | met (#393)  | `TestHostFormAuthenticates`, `TestHostFormCapability`, `TestHostFormAtespacesLimit`                                                                                                                                                                                                                                                                                             |
| PP-10 | The hostname form refuses with 403 a state-changing request or a WebSocket handshake that a browser sends from another origin, also from another environment's hostname under the same suffix. The request's own origin and the origins in `--port-proxy-trusted-origins` pass, as do requests with no browser headers.                                                                                                                                                                                                                                                                                                                                     | met (#393)  | `TestHostFormRefusesCrossOrigin`                                                                                                                                                                                                                                                                                                                                                |
| PP-11 | What the other forms do holds for the hostname form: `X-Env-Wake-Wait` holds the request (`PP-3`) and does not reach the daemon; a waking environment gets 503 with `Retry-After: 1`, a crashed one 410 and a deleted or deleting one 404, with the same bodies; a WebSocket upgrade passes and bytes flow both ways; a streamed response reaches the caller as the daemon writes it.                                                                                                                                                                                                                                                                       | met (#393)  | `TestHostFormWakeWait`, `TestHostFormAnswersForAnEnvironmentThatDoesNotServe`, `TestHostFormUpgradeAndStreaming`                                                                                                                                                                                                                                                                |
| PP-12 | `ate-env manifest --port-proxy-host-suffix <domain>` renders `-port-proxy-host-suffix=<domain>` on the `ate-env-api` Deployment and no Ingress, DNS or certificate object. Without the flag the argument is absent. A value that is not a domain name, or that has one label (`com`), fails the command, and fails `ate-env-api` at start.                                                                                                                                                                                                                                                                                                                  | met (#393)  | `TestManifestPortProxyHostSuffix` (`cmd/ate-env`), `TestNormalizeHostSuffix`                                                                                                                                                                                                                                                                                                    |
| PP-13 | `ate-env-api` does not start, and `ate-env manifest` fails, when the suffix is a name that callers use for the API or is above one: `localhost`, an in-cluster name of the Service, or a name in `--allowed-hosts` (`--api-allowed-hosts` on the manifest), a wildcard entry by the name behind its `*.`. The API would answer 400 on such a name while a health probe that sends an IP address still passes. A suffix next to those names, or below a wildcard entry, passes. `ate-env-api` also does not start with a suffix when the port proxy is off (`--atenet-connect` empty): nothing would serve the names.                                        | met (#393)  | `TestCheckHostSuffix`, `TestAPIHosts`, `TestCheckPortProxyHostSuffix` (`cmd/ate-env-api`), `TestManifestPortProxyHostSuffix` (`cmd/ate-env`)                                                                                                                                                                                                                                    |

## Hostname form

The decisions behind `PP-4` to `PP-13` (issue #388).

- **Authentication.** The rule of the path form: the caller token, or a port capability on a
  WebSocket handshake. The form adds no credential and no public port.
- **Atespace.** An environment id is unique only inside an atespace. An atespace and an id are both
  DNS labels (`label` in `internal/portproxy/portproxy.go`), thus the atespace is one more label:
  `<port>-<id>.<atespace>.<domain>`. The name without it, `<port>-<id>.<domain>`, means the default
  atespace, as a request without `X-Env-Atespace` and the short path do. The count of labels in
  front of the suffix tells the two apart, thus no name has two readings. A wildcard certificate
  covers one label: `*.<domain>` serves the short names, and each atespace needs
  `*.<atespace>.<domain>`.
- **One form.** A name under the suffix names an environment port and nothing else. The API has no
  route there, and the routing headers are refused instead of merged or ignored. The path is the
  daemon's, thus the proxy does not read it.
- **A domain of its own.** The suffix and each name below it serve environment ports only, thus the
  API needs a name outside it. `ate-env-api` refuses a suffix that covers one of its own names at
  start (`PP-13`), because a health probe by IP address would not show the fault. It cannot see a
  name that only a front proxy or a gRPC client uses.
- **DNS limits.** The first label holds the port, a `-` and the id, and a DNS label has at most 63
  bytes. An id of more than 57 characters cannot be named with a port of five figures; the header
  form and the path form reach it.
- **Out of scope.** `ate-env manifest` renders no Ingress, DNS record or certificate. The operator
  provides wildcard DNS for the suffix and, for `https`, wildcard certificates in front of
  `ate-env-api`, and the front must pass `Host` unchanged. `ate_env_e2b.Sandbox.get_host`,
  `download_url` and `upload_url` (`E2B-33`) are a later pull request.

## What the property test found

- **PP-1.** When the path form's rest held no percent-escape, `parsePath` dropped the raw path (it
  equalled the decoded one), and Go's `url.EscapedPath` re-escaped the characters it escapes in a
  path though they are valid as sent: `/!` reached the daemon as `/%21` and `/files/report(1).txt`
  as `/files/report%281%29.txt`. The header form delivered them unchanged. Found by
  `TestPathFormDeliversPathAsSentProperty`, which generates paths from unreserved characters,
  sub-delims, `:` `@` and percent-escapes and sends each through both forms; the example tests used
  unreserved characters and escapes only, and the bug needs a sub-delim with no escape anywhere in
  the rest.
- **PP-1, again.** Go's server accepts bytes in a request-target that `url.EscapedPath` calls
  invalid in a raw path (`{ } | ^ " < > \`, `` ` ``, `#` and raw UTF-8). The server keeps them in
  `URL.RawPath`, but `EscapedPath` drops such a `RawPath` and re-escapes the decoded path, in both
  forms: `/a{b}` reached the daemon as `/a%7Bb%7D`, and `/a{b%2Fc` as `/a%7Bb/c`, with one segment
  more. The path form also matched its routing prefix on that re-escaped path, so an escaped slash
  routed: `/v1alpha%2Fenvs%2Fenv1%2Fports%2F8000/a{` reached the daemon. The proxy now reads the
  path as sent from `URL.RawPath`, matches the prefix on it, and writes it to the daemon as the
  request-target. A path that starts with `//` would read as an authority if written as is, so in it
  only those bytes are escaped and the segments stay. Found by an audit of the criterion: the
  property test sent its paths through `http.Client`, which escapes these bytes itself, and
  generated none of them. It now writes each request line on a raw TCP connection, generates these
  bytes mixed with `%2F`, a leading `//` and a query, and also sends the routing prefix with one
  separator escaped, which must reach no daemon. On the old code it fails at once, shrunk to `/{`.
- **PP-2.** `httputil.ReverseProxy` runs `cleanQueryParams` on the outbound `URL.RawQuery` before
  calling `Rewrite`. When a query holds `;` or a bad `%` escape, `cleanQueryParams` re-encodes it
  through `url.ParseQuery`, which drops parameters with `;` or bad escapes and sorts and re-encodes
  the rest: `/?;` reached the daemon as `/`, `/x?a=1;b=2` as `/x`, and `/?z=1&a=2%203&semi;drop` as
  `/?a=2+3&z=1`. The proxy never inspects the query, so `rewrite` copies `RawQuery` from
  `pr.In.URL`; `ReverseProxy` keeps `ForceQuery` (a bare `?`) itself.
  `TestPathFormDeliversPathAsSentProperty` now draws bare `?`, `;`, `&`, `/`, `?`, bad `%` escapes,
  `{ } | ^`, `#`, and raw UTF-8 in queries on ordinary and `//` paths (and after a bare path-form
  prefix), and `TestRawQueryKeptAsSent` checks the examples across both forms. On the previous code
  `rapid` shrinks the failure to `/?;`.

It is not a timing bug, so `testing/synctest` alone would not have found it: synctest makes
scheduling deterministic but generates no inputs and checks nothing.

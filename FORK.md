# PandaFan Hysteria fork

Upstream: `apernet/hysteria`, master `619a6f8` (2026-09-06).
Publishing branch: `rankjie/hysteria`, `master`.

## Credential revocation

File authentication validates and registers each QUIC connection atomically with
respect to snapshot replacement. A successful file reload closes every registered
connection whose user was removed or whose password changed. Equivalent snapshots
keep sessions; malformed snapshots keep the last valid configuration and sessions.
The watcher and explicit reload use this same path.

Registration happens before traffic-logger registration. Thus a concurrent reload
cannot miss a connection that has authenticated but is not visible to `/kick` yet.
Disconnect cleanup unregisters sessions, and revocation callbacks run outside the
session lock. This works without a traffic logger and closes the whole QUIC
connection, including existing TCP streams and UDP sessions.

The existing `/kick` endpoint still closes all tracked connections for requested
IDs. It is useful for operational kicks; file credential invalidation no longer
depends on the endpoint being reachable or on its registration timing. Other
authentication backends retain their existing contracts.

Preserved fork behavior includes reloadable file authentication, upload/download
usage logs, active-session kick tracking, lower per-connection memory usage, and
stable-path CI artifact publishing. Control-plane RPC callers remain asynchronous.

## Build identification

`hysteria version` and `hysteria -v` print `PandaFan modified 2026-09-06`.
This source marker is independent of release ldflags. The date identifies the
fork source update; full version output also includes CI build date and commit.
The master build checks both commands before publishing artifacts.

## Validation

Regression tests reproduce the authentication-to-registration window using a real
QUIC connection. Real TCP tunnels verify that changed users disconnect while
unchanged users retain their connections, replacement credentials connect, old
credentials fail, and deletion disconnects all users. Session tests cover failed
reloads, equivalent reloads, concurrent registration, repeated cleanup, and old
cleanup after a new credential generation has registered.

These tests and CI builds do not establish which binary is running on production.

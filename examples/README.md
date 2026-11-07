# Examples

`review.json` is a Job template for the review kind, in the shape
[`docs/jobs.md`](../docs/jobs.md) describes: the three containers, the volumes
and their read-only mounts, the Secrets by name, the placeholders the poller
fills in. It is a starting point for the manifests in the maintainer's private
repository, not a manifest itself: the private patterns Secret, the hold volume
and the image digests are the lab's to set. The App key Secret carries two keys,
`client-id` and `private-key.pem`, so the client ID comes from the same Secret as
the key; `twr-private-patterns` is mounted where `PUBLISH_PATTERNS_FILE` points and
is optional, so a Job runs without it (with no extra patterns). The poller's own
CronJob is not here; its idle gate for the lab is ecu's `/healthz` with
`POLL_IDLE_GATE_FIELD=idle`. `internal/poller`'s tests render it
to check it stays well-formed.

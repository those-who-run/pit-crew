# Examples

`review.json` is a Job template for the review kind, in the shape
[`docs/jobs.md`](../docs/jobs.md) describes: the three containers, the volumes
and their read-only mounts, the Secrets by name, the placeholders the poller
fills in. It is a starting point for the manifests in the maintainer's private
repository, not a manifest itself: the private patterns Secret, the hold volume
and the image digests are the lab's to set. `internal/poller`'s tests render it
to check it stays well-formed.

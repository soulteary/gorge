# Conpherence notification retries

The native notification worker previously accepted only feed notifications
(`type: notification`). Phorge also uses `type: message` for live room updates.
The paired post-commit fix in [Phorge #61](https://github.com/soulteary/phorge/pull/61)
can enqueue those room events when synchronous publication fails.

Accept room events only when they contain a nonempty Conpherence PHID, a positive
integer transaction ID, and exactly one subscription equal to that room PHID.
Unknown types, malformed identity, mismatched or extra recipients remain
permanent failures without network publication. Existing feed validation,
instance policy, stable deduplication IDs, retry policy and endpoint handling
remain in force. No PHP worker is reintroduced.

The regression test sends a room event through the real native handler to a
local HTTP endpoint, injects a required-mode failure, and verifies a successful
retry with unchanged identity and instance. Negative cases verify that invalid
events never reach the network. Existing feed notification tests also run.

Deploy the compatible Gorge worker before relying on Phorge's queued room retry.
This does not create a transactional room outbox: a simultaneous failure of
Phorge's synchronous publication and queue submission can still lose the live
notification, while the committed room message remains available on reload.

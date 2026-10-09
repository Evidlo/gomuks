# Patched copy of mautrix-go v0.11 (UGLY HACK)

This is a vendored copy of `maunium.net/go/mautrix v0.11.2-0.20240620211416-fa19263891f5`, wired in via a
`replace` directive in gomuks' `go.mod`. It exists only so that interactive (emoji) verification works with
modern clients. Delete this directory, the `replace` line, and `matrix/sascompat` when upgrading to mautrix v0.18+,
whose `crypto/verificationhelper` implements the whole flow properly.

Changes (search for `GOMUKS PATCH`):

- `crypto/decryptolm.go`: decrypted olm payloads are parsed as to-device events, so encrypted verification
  events (which modern clients send) get parsed instead of left as unknown content.
- `crypto/machine.go`: decrypted to-device events that aren't keys are dispatched through `HandleToDeviceEvent`
  like plaintext ones, instead of being dropped as "unhandled".
- `crypto/machine.go`: new `OnToDeviceEvent` hook, used by `matrix/sascompat` to see `m.key.verification.ready`
  and `cancel` events, which v0.11 doesn't handle itself.

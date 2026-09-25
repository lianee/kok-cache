# Code signing policy

Free code signing provided by [SignPath.io](https://signpath.io), certificate by
[SignPath Foundation](https://signpath.org).

## Scope

Only the Windows executable `kok-cache-windows-amd64.exe` is signed. It is built from this
repository by the public [release workflow](.github/workflows/release.yml), from a tagged commit
of the `public` branch, and nothing else is signed with this certificate.

## Team roles

kok-cache is maintained by a single person, who holds all three roles:

- Committers and reviewers: [lianee](https://github.com/lianee)
- Approvers: [lianee](https://github.com/lianee)

Every release is approved manually before it is signed.

## Verifying a release

Every artifact carries a GitHub build provenance attestation, and every build is reproducible.
[TECHNIQUE.md](TECHNIQUE.md) explains how to rebuild and compare. The Authenticode signature is
embedded in the Windows executable, so remove it before comparing that file with a local build.

## Privacy

This program sends data to the k-ok server, to other connected players, to a Google STUN server
and to GitHub. What is sent and why is described in the [privacy policy](PRIVACY.md) (French).

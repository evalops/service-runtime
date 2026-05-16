# Bazel Build

`service-runtime` has a small Bazel surface for Go package build/test coverage
and for proving the shared Buildfarm path before moving heavier legacy checks.
The existing Go test and release workflows remain the release path.

## Local Checks

After adding or moving Go packages, regenerate checked-in Bazel targets:

```sh
make bazel-gazelle
```

Run the local Bazel graph:

```sh
make bazel-test
```

Check graph hygiene before opening a PR:

```sh
make bazel-check
```

## Dev Buildfarm Remote Execution

The `remote-gcp-dev` Bazel config points at the Deploy-owned
`bazel-rbe-dev-buildfarm` backend. Locally, the helper opens a short-lived
SSH/IAP tunnel to the same Buildfarm listener used by colocated CI runners:

```sh
make bazel-rbe-smoke
```

Trusted CI should run on the repo-scoped Buildfarm runner label
`evalops-service-runtime-rbe` after Deploy has registered that runner through
`additional_bazel_buildfarm_runners`.

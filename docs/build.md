# Kubemap build and release notes

How the images and release files are built.

- **Server** (`cmd/kubemap-portal`, `portal/`, `web/`): one Deployment `kubemap` in the cluster it shows. It serves the embedded React UI over plain HTTP (TLS ends at Traefik), keeps SQLite on a PVC, and runs the cluster-agent logic in-process (`portal/localcluster/`).
- **Node agent** (`cmd/kubemap-agent`, `agent/`): a DaemonSet `kubemap-node` from the same image. It reports node health to the server over the cluster-internal Service; the server checks its ServiceAccount token with a TokenReview.
- **Protocol** (`pkg/protocol/`): messages shared by both.

Install: `install.md`. Design: `architecture-web.md`.

The published image is built by GitHub Actions (`.github/workflows/image.yml`) and pushed to `ghcr.io/albertycl/kubemap`. The `make push` target below is the older GitLab route and is not used any more.

## Build

```
make            # fmt-check, vet, test, web UI, server and node agent binaries (linux)
make image      # container image with the server and the node agent (deploy/Dockerfile; base images pinned by digest)
make smoke      # check that both binaries run and agree on the version
make release VERSION=0.1.0   # everything for an install, into dist/ (below)
make push IMAGE=registry.gitlab.com/<account>/kubemap VERSION=0.1.0
```

`make release VERSION=x.y.z` (KM-13) runs only from a clean checkout that has the web UI sources, and stops at the first failure. It runs `make all`, checks that both binaries report the version, runs `make smoke`, builds the image and checks its version label and user 65532, and checks that the build changed no tracked file. Then it writes `dist/`:

| File | What |
|---|---|
| `kubemap-<v>.yaml` | The install manifest for this image (`kubemap-agent bundle`). |
| `kubemap-<v>-image.tar.gz` | The image (`docker save`). Until there is a registry, import it on the node with `sudo k3s ctr images import`. |
| `IMAGE.txt` | Image tag and image ID. |
| `RELEASE.txt` | Version, commit, Go version, image. |
| `SHA256SUMS` | Checksums of the files above. |

`SKIP_IMAGE=1` leaves the image out (no Docker); `RELEASE.txt` then says the release is not complete. `make push` refuses any `IMAGE` that is not `registry.gitlab.com/<account>/...`; log in with `docker login registry.gitlab.com` first. No registry credentials are kept in the repository.

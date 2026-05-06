# Release Packages

Firedoze release tags publish installable Linux packages as GitHub Release
artifacts. There is no APT/YUM repository yet.

The current packages are for `linux/amd64` hosts. They install:

- `firedoze`, `firedozed`, and `firedoze-image-builder` to `/usr/bin`.
- The pinned upstream Firecracker VMM binary to `/usr/lib/firedoze/firecracker`.
- The packaged Linux guest helper used by `firedoze-image-builder` to
  `/usr/lib/firedoze`.
- The `firedozed` systemd unit to `/usr/lib/systemd/system`.
- The example config to `/etc/firedoze/firedoze.example.toml`.
- Documentation to `/usr/share/doc/firedoze`.
- The `firedoze` system user and standard config/state/log directories.

The packages do **not** create your real config, build the base image, or start
the daemon. Follow the [admin quickstart](quickstart-admin.md) for those host
setup steps after installing the package.

## Verify A Release

Download the package, checksum file, and Sigstore bundle from the GitHub Release:

```sh
version=0.1.0
tag="v$version"

curl -LO "https://github.com/addrummond/firedoze/releases/download/$tag/firedoze_${version}_linux_amd64.deb"
curl -LO "https://github.com/addrummond/firedoze/releases/download/$tag/firedoze_${version}_checksums.txt"
curl -LO "https://github.com/addrummond/firedoze/releases/download/$tag/firedoze_${version}_checksums.txt.bundle"
```

Verify that the checksum file was signed by the Firedoze release workflow:

```sh
cosign verify-blob \
  --bundle "firedoze_${version}_checksums.txt.bundle" \
  --certificate-identity "https://github.com/addrummond/firedoze/.github/workflows/release.yml@refs/tags/$tag" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
  "firedoze_${version}_checksums.txt"
```

Then verify the package checksum:

```sh
sha256sum --ignore-missing -c "firedoze_${version}_checksums.txt"
```

On macOS or another system without GNU `sha256sum`, use:

```sh
shasum -a 256 --ignore-missing -c "firedoze_${version}_checksums.txt"
```

## Install A Package

On Debian or Ubuntu:

```sh
sudo apt install "./firedoze_${version}_linux_amd64.deb"
```

On RPM-based distributions:

```sh
sudo dnf install "./firedoze_${version}_linux_amd64.rpm"
```

After installation, continue with the admin quickstart from the base image,
config, and peer setup steps.

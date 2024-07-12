# kubectl-quadlet

create quadlet files from kubernetes manifests

## usage

```sh
k quadlet -k vaultwarden/overlays/digitalocean
```

Outputs:

* [`vaultwarden.kube`](https://docs.podman.io/en/latest/markdown/podman-systemd.unit.5.html#kube-units-kube)
* `vaultwarden.full_manifest.yaml`
  * technically isn't the full manifest; we may omit k8s kinds that `podman kube play` won't understand

You should structure these outputs like so:

```text
$XDG_CONFIG_HOME/containers/systemd
├── vaultwarden.kube
└── vaultwarden/
    └── vaultwarden.full_manifest.yaml
```

## install

### goreleaser

```sh
goreleaser build --single-target --clean
ln -sf $(realpath .)/dist/??/kubectl-quadlet ~/.local/bin
```

### tap

```sh
brew tap guppy0130/kubectl-quadlet https://github.com/guppy0130/kubectl-quadlet
brew install kubectl-quadlet
```

### krew

```sh
kubectl krew install quadlet
```

* you may need to configure where you get your manifests from

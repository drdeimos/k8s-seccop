# About

Copy secrets with label `secret-copier` to all namespaces

# Project status

Deep work in progress. Really, **do not use it now!**

# Usage

```shell
git clone https://github.com/drdeimos/k8s-seccop.git
cd k8s-seccop
go mod download
go build seccop.go
export KUBECONFIG=<path_to_your_kubernetes_config>
./seccop
```

# TODO

- [x] Read in-cluster config
- [x] Do not try overwrite already copied secret
- [ ] Copy secret to all namespaces (now `production` hardcoded)
- [ ] Check relevance already exist secret
- [ ] Fix `go get`
- [ ] Deployment example
- [ ] Work queue?

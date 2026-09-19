# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

The shared Go library imported by every service that `devkit ngs` generates.
Three names to keep straight:

- Go module path: `github.com/sezznaw/devkit-common`
- GitHub repository: `sezznaw/devkit-common` (public)
- Local directory: `common` (sibling of `../devkit` and `../devkit-registry`)

It is a runtime dependency of production services, which is why it is its own
module rather than part of the devkit CLI or the template registry.

## Commands

```sh
go test ./...
go test ./kitexx/ -run TestLoggingMiddleware    # single test
go vet ./... && test -z "$(gofmt -l .)"         # what CI runs
```

## Packages

- `log`: slog wrapper. `New(Options)` also installs the slog default;
  `WithContext`/`FromContext` carry a request-scoped logger; `Options` is
  YAML-loadable (its `Output` field is not).
- `config`: `Load(dir, &cfg)` reads `<dir>/<APP_ENV>.yaml` (default `dev`)
  and expands `${VAR}` from the environment *before* YAML parsing.
- `kitexx`: the glue a service's `main` uses. `Options(cfg)` returns Kitex
  server options (basic info, listen address, logging middleware, Nacos
  registry unless `registry_disabled`), `ClientOptions(cfg)` returns the Nacos
  resolver, `Run` starts the server. `kitexx.Config` is meant to be embedded
  inline in the service's own config struct; the `kitex-service` template in
  `../devkit-registry` depends on its field names and YAML keys.
- `nacosx`, `redisx`, `etcdx`: thin constructors from YAML-loadable config
  structs; `redisx.New` pings before returning.

## Versioning rules

- Tags are plain semver (`v0.1.0`). Once a tag is pushed, proxy.golang.org
  caches that version forever: never move or re-push a tag, publish the next
  version instead. `v0.1.0` is already cached.
- While on 0.x, a breaking change bumps the minor version.
- The `kitex-service` template pins `CommonVersion` and `KitexVersion`. After
  tagging a release here, bump those defaults in
  `../devkit-registry/components/kitex-service/component.json` and publish a
  new template version, otherwise new services keep getting the old common.
- The Kitex version in `go.mod` and the template's `KitexVersion` (which also
  selects the `kitex` code generator version) must match.

## Gotchas

- `go mod tidy` needs `google.golang.org/genproto` at a recent version;
  nacos-sdk-go pulls an old monolithic genproto that collides with the split
  modules ("ambiguous import"). If that error comes back after a dependency
  bump, `go get google.golang.org/genproto@latest` fixes it.
- Docs come in pairs: `README.md` and `README.zh-CN.md`, same structure.

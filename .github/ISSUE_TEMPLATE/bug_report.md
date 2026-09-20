---
name: Bug report
about: Something the plugin does wrong
title: ""
labels: bug
assignees: ""
---

<!-- Please do not report security vulnerabilities here. See SECURITY.md. -->

## Environment

- **CLIProxyAPI version:**
- **Was it built with cgo?** (a `CGO_ENABLED=0` build cannot load any plugin)
- **Plugin version:** (the `Version` in the plugin's registration metadata, or the release
  tag you installed; a locally built one reports `0.1.0-dev`)
- **OS and architecture:** (for example `linux/amd64`, `darwin/arm64`)
- **Installed from:** plugin store / manual build

## Plugin configuration

The `plugins.configs.local-llm-pool` block, **with API keys and any other secrets removed**.

```yaml

```

If the problem involves routing, the relevant `openai-compatibility` providers help too,
again without keys.

## Status output

`GET /v0/resource/plugins/local-llm-pool/status`, taken while the problem is happening.

```json

```

## Host log lines

Relevant lines from the CLIProxyAPI log, especially anything mentioning `pluginhost`,
`scheduler` or the plugin id.

```

```

## Expected behaviour

What you expected the plugin to do.

## Actual behaviour

What it did instead. If a request went to the wrong backend, say which one you expected and
which one served it.

## Steps to reproduce

1.
2.
3.

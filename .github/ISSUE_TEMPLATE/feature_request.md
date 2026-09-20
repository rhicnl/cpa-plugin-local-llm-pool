---
name: Feature request
about: Suggest a capability or a configuration option
title: ""
labels: enhancement
assignees: ""
---

## Problem

What are you trying to do that the plugin makes hard or impossible today? Describe the
situation rather than the solution, so the fix is not constrained to one shape.

## Proposed behaviour

What should the plugin do? If it changes routing, say what happens when backends are free,
when they are full, and when one is unreachable.

## Configuration sketch

How would this be configured? A rough YAML block is enough.

```yaml
plugins:
  configs:
    local-llm-pool:
      enabled: true
      # ...
```

Note whether this is a new key, a change to an existing one, or a new value for an existing
key. Existing config keys and `/status` keys have to keep working, so say if your proposal
cannot avoid breaking one.

## Alternatives

What you tried or considered instead, including whether it can already be done with the
current configuration, with a second plugin, or on the proxy side.

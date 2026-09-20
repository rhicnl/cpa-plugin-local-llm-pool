# Security Policy

## Reporting a vulnerability

**Please do not open a public issue for a security vulnerability.**

Report it privately through GitHub's private vulnerability reporting: go to the
[Security tab](https://github.com/rhicnl/cpa-plugin-local-llm-pool/security) of
this repository and choose **Report a vulnerability**. That opens a private advisory visible
only to you and the maintainer.

Useful things to include: what an attacker can do, the affected plugin version, the
CLIProxyAPI version, and the smallest configuration or request sequence that reproduces it.

## Supported versions

Only the latest release is supported. Fixes go out in a new release rather than as patches
to older tags.

## What you are installing

A CLIProxyAPI plugin is a **native dynamic library loaded into the proxy process with
`dlopen`**. There is no sandbox and no privilege boundary. A plugin runs with everything the
proxy has: its memory, its file descriptors, its network access and its credentials. This is
true of every plugin in the ecosystem, not just this one.

Install plugins only from sources you trust, and read the source when you can. For this
plugin specifically:

- It makes no network calls of its own.
- It writes no files.
- It emits no log lines, so it cannot leak request content or credentials into host logs.
- It holds only counters, an auth ID to backend mapping, and its own configuration.

## The status resource

`/v0/resource/plugins/local-llm-pool/status` is a browser-navigable plugin resource, which
means the host does **not** require management authentication for it. It exposes backend
names, the attributes they match on, and concurrency counts. It never exposes credentials,
and the host strips sensitive attributes such as API keys, tokens and proxy URLs before
handing candidates to a scheduler plugin. If your backend names themselves are sensitive,
keep the management port off untrusted networks.

---
name: hello-checks
description: Use when the user starts using the npower-validacao power (including "Try power") or asks to run the validation checks of this kit. It checks the kit's pretend installation and runs the validation tools.
---

# npower-validacao: validation kit

> **Kit version 0.1.1.** This line only exists in 0.1.1: if you can read it, Kiro applied the update of the SKILL text.

This power only exists to test how Kiro runs a Power. It has no product behavior.

## First use ("Try power")

1. Call `hello_status` (server `hello` of this power).
2. If `in_sync` is `false`, call `hello_install` right away. Do not ask the user for approval first.
3. Tell the user in one sentence that the kit is ready, and which tools you called.

## Checks the user may ask for

- "ping": call `ping`.
- "info": call `info` and show the result as it is.
- "marker": call `write_marker` with the text the user gives, then `read_marker`.
- "log": call `log_tail`.
- "restart hot", "restart silent", "restart next-start", "restart reset": call `restart` with mode `hot`, `hot-silent`, `next-start` or `reset`. After a hot restart, list the tools you can see and say whether a `hello_gen<N>` tool exists.

Always show tool results exactly as returned. Never edit files of this power.

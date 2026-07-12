# Extensions

Pi supports [extensions](https://github.com/badlogic/pi-mono/blob/main/packages/coding-agent/docs/extensions.md)
— TypeScript modules that hook into the agent lifecycle, register custom tools,
and modify behavior. OpenCrow passes extension paths to pi via a generated
`settings.json` in `PI_CODING_AGENT_DIR`.

## NixOS module

The NixOS module provides a declarative `extensions` option. Set a value to
`true` to enable a packaged extension that ships with the opencrow flake, or
pass a path for a custom extension:

```nix
services.opencrow.extensions = {
  reminders = true;                  # packaged extension (resolved from flake)
  my-ext = ./extensions/my-ext.ts;   # custom extension
};
```

Setting a value to `false` explicitly disables an extension (useful for
overriding defaults from other modules). The attrset is mergeable across NixOS
module files.

Extra keys for pi's `settings.json` (e.g. `packages`, `compaction`) can be
added via `piSettings`:

```nix
services.opencrow.piSettings = {
  compaction.enabled = true;
};
```

## Packaged extensions

### reminders

Structured tools for scheduling, listing, and cancelling one-shot reminders.
The SQLite binary is patched into the extension at build time, so it does not
need to be added to `extraPackages`.

```nix
services.opencrow.extensions.reminders = true;
```

See [Heartbeat & Reminders](heartbeat.md) for usage details and
[`extensions/reminders/`](../extensions/reminders/) for the source.

## Writing an extension

See the [pi extensions documentation](https://github.com/badlogic/pi-mono/blob/main/packages/coding-agent/docs/extensions.md)
for the full API. In short, create a TypeScript file that exports a default
function receiving the `ExtensionAPI`:

```typescript
import type { ExtensionAPI } from "@mariozechner/pi-coding-agent";

export default function (pi: ExtensionAPI) {
  pi.on("agent_end", async (event) => {
    // react to agent lifecycle events
  });

  pi.registerTool({
    name: "my_tool",
    // register custom tools callable by the LLM
  });
}
```

To package an extension for the NixOS module, add it under `extensions/<name>/`
with an `index.ts` entry point, create a package in `nix/`, and expose it in
`flake.nix` as `extension-<name>`. The module resolves `extensions.<name> = true`
to the corresponding flake package output.

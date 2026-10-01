# OpenSBX

Run code you do not trust in disposable local sandboxes.

OpenSBX provides a local CLI, REST API and MCP tools to create sandboxes, run commands,
manage files, and expose app ports on your local machine.

**Start here:** [Use OpenSBX](docs/usage.md) · [Choose a runtime](docs/runtimes.md) · [Contribute](CONTRIBUTING.md) · [Architecture](docs/architecture.md)

## Quick start

Requirements: Go **1.25.13+** only when building from source, and a running local
[Docker runtime or supported Apple container runtime](docs/runtimes.md).

```sh
curl -fsSL https://raw.githubusercontent.com/MrUprizing/opensbx/main/scripts/install.sh | bash
opensbx start
```

In another terminal, prepare an image, create a sandbox, run a command and delete
it when finished. No HTTP snippets or JSON tooling are needed:

```sh
opensbx image pull node:22

SANDBOX_ID=$(opensbx create node:22 --ttl 15m --quiet)
opensbx ls
opensbx exec "$SANDBOX_ID" -- node --version
opensbx rm "$SANDBOX_ID"
```

Stop the server gracefully when you are done:

```sh
opensbx stop
```

Image pulls are explicit; images already cached by the runtime are not imported
automatically. The six everyday commands are `create`, `ls`, `inspect`, `exec`,
`logs` and `rm`; `sandbox`, `command`, `file` and `image` expose the full groups.
Use `opensbx help exec` for options. Management never auto-starts the server.
`opensbx stop` stops the daemon; `opensbx sandbox stop TARGET` stops one sandbox.
The CLI uses Cobra for command discovery, contextual help, interspersed flags and
native Bash/Zsh/Fish completion; run `opensbx completion --help` for setup commands.
For configuration, image import/export, completion, MCP and migration, see
[Usage](docs/usage.md). The API reference is available at
`http://localhost:18089/swagger/index.html` while the server is running.

Command signals use verified guest PID/start-time identity, never argv matching
or guessed host PIDs. Docker prepares identity when shell/proc support is available;
shell-free images still execute directly, with signals unavailable for those commands.
Logs have bounded retention, and foreground
JSON capture is limited to 8 MiB total; see [output and safety limits](docs/usage.md#commands-output-and-interruption).

## Sponsors

Thanks to these amazing people for supporting this project:

<table>
  <tr>
    <td align="center">
      <a href="https://github.com/camilocbarrera">
        <img src="https://avatars.githubusercontent.com/u/85809276?v=4" width="80" alt="camilocbarrera" /><br />
        <sub><b>Cris</b></sub>
      </a>
    </td>
    <td align="center">
      <a href="https://github.com/cuevaio">
        <img src="https://avatars.githubusercontent.com/u/83598208?v=4" width="80" alt="cuevaio" /><br />
        <sub><b>anthony</b></sub>
      </a>
    </td>
  </tr>
</table>

Become a sponsor on [GitHub Sponsors](https://github.com/sponsors/MrUprizing).

## License

[Apache License 2.0](LICENSE)

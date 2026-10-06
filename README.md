# gossh

A terminal UI for managing and connecting to SSH hosts in `~/.ssh/config`.

## Install

No Go toolchain needed. The installer picks the right binary for your OS and CPU,
puts `gossh` on your PATH, and creates `~/.ssh/config` and `~/.ssh/keys/`.

**Windows (x64 / ARM64)**, in PowerShell:

```powershell
irm https://raw.githubusercontent.com/JiaBao-do/gossh/main/install.ps1 | iex
```

**Linux / macOS (amd64 / arm64)**:

```sh
curl -fsSL https://raw.githubusercontent.com/JiaBao-do/gossh/main/install.sh | sh
```

Open a new terminal and run `gossh`.

| | Windows | Linux / macOS |
|---|---|---|
| Binary | `%LOCALAPPDATA%\Programs\gossh\gossh.exe` | `~/.local/bin/gossh` |
| PATH | user PATH | `~/.bashrc`, `~/.bash_profile` (macOS), `~/.zshrc`, fish `config.fish` or `~/.profile` |
| Admin / sudo | not needed | not needed |

Options (environment variables, or flags on the script):

| Variable | Flag (sh / ps1) | Meaning |
|---|---|---|
| `GOSSH_VERSION=v1.2.0` | `--version` / `-Version` | install a specific release |
| `GOSSH_INSTALL_DIR=...` | `--dir` / `-InstallDir` | install somewhere else |
| `GOSSH_UNINSTALL=1` | `--uninstall` / `-Uninstall` | remove gossh and its PATH entry |
| `GOSSH_NO_MODIFY_PATH=1` | | leave PATH alone |

**Offline install:** download the release files (the binary for your platform
plus `install.sh` / `install.ps1`) into one folder and run the script there. It
uses the binary next to it instead of downloading.

## Usage

```
gossh                    interactive host picker
                         (enter = connect, f = forward, e = edit, a = add, / = filter)
gossh prod-db            connect to a saved host
gossh prod-db -A         any ssh flags pass straight through
gossh user@1.2.3.4       plain ssh
gossh list | add | edit [alias] | delete
gossh version
```

### Port forwarding

Run `gossh forward` (or press `f` in the picker) for a guided setup, or give
the specs directly:

```
gossh forward prod-db 8080              localhost:8080 -> localhost:8080 on prod-db
gossh forward prod-db 5433:5432         localhost:5433 -> localhost:5432 on prod-db
gossh forward prod-db 5433:db.internal:5432   reach a host only prod-db can see
gossh forward prod-db R:9000:3000       prod-db:9000 -> your localhost:3000
gossh forward prod-db D:1080            SOCKS5 proxy on localhost:1080
gossh forward prod-db 8080:80 D:1080    several at once
```

Ctrl+C stops it. Busy local ports are reported before connecting.

### Keys

All gossh-managed keys live in `~/.ssh/keys/`. The IdentityFile field accepts
a key name from that folder (Tab autocompletes) or a path to any key file.

**Existing configs are left alone.** Hosts that already point at keys
elsewhere keep working and are not changed until you decide:

- When you enter a key outside `~/.ssh/keys` you choose **Copy** (keep the
  original), **Move** (remove the original) or **Keep** (reference it where it is).
- `gossh keys migrate` lists every host using an outside key and lets you
  pick hosts, then Copy / Move / Keep in one go. A key still used by another
  host (or a `Host *` / `Match` block) is never deleted.

```
gossh keys                              list managed keys
gossh keys import ~/Downloads/prod.pem  copy a key in (add --move to move it)
gossh keys migrate                      bring existing hosts' keys into ~/.ssh/keys
```

Imported keys get owner-only permissions (on Windows the ACL is locked down
too, which OpenSSH requires) and their `.pub` is brought along. The config
references `~/.ssh/keys/<name>`, which works on every OS.

### Your existing ~/.ssh/config is safe

gossh only edits `Host <alias>` blocks and only the HostName, User, Port and
IdentityFile lines in them. Everything else (`Include`, `Host *`, `Match`,
`ProxyJump`, `LocalForward`, comments, formatting) is kept exactly as is,
and the previous file is saved to `~/.ssh/config.bak` before every change.

## Development

Requires Go 1.26+, GNU make and `sh` (Git Bash on Windows).

```
make                 build bin/gossh for this machine
make run ARGS=list   go run with arguments
make sandbox         run with HOME set to ./.sandbox (your real ~/.ssh is untouched)
make tools           install Delve
make debug           headless Delve on :2345; attach with `dlv connect :2345` or your IDE
make debug-build     unoptimised binary for your debugger
make install         build and install this checkout onto PATH
make uninstall
make dist            cross-compile all 6 platforms + checksums into dist/
make test vet fmt tidy upgrade clean
```

### Releasing

```
git tag v1.0.0 && git push origin v1.0.0
```

The `release` workflow builds every platform and publishes a GitHub release,
which is what the install scripts download.
